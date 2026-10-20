package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"paepcke.de/goproxy/internal/store"
)

const (
	// PrewarmLimit caps how many of the hottest modules one sync refreshes:
	// enough to keep the fleet's common builds warm without turning the
	// proxy into a crawler.
	PrewarmLimit = 100
	// PrewarmInterval is the nominal sync cadence. Each pass is jittered
	// by PrewarmJitter, so a fleet of proxies does not hit the upstream in
	// lockstep.
	PrewarmInterval = time.Hour
	// PrewarmJitter is the symmetric randomness added to each interval.
	PrewarmJitter = 10 * time.Minute

	prewarmWorkers = 4
	prewarmTimeout = 10 * time.Minute
)

// WarmSummary is the outcome of one cache sync pass.
type WarmSummary struct {
	Modules int   // modules considered
	Fetched int   // modules whose files were downloaded (not already cached)
	Bytes   int64 // upstream or neighbour bytes pulled
	Failed  int   // modules that could not be resolved or fetched
}

// Warmer keeps the hottest modules of a fleet warm. At startup and then
// once per jittered hour it resolves the latest version of the most
// requested modules and fetches their files through the regular fetch path,
// so the neighbour caches are asked first and one download serves the whole
// fleet. Everything it stores is verified exactly like a client request.
type Warmer struct {
	p        *Proxy
	popular  func(limit int) []string
	interval time.Duration
	jitter   time.Duration
	log      *slog.Logger
}

// NewWarmer creates a warmer. popular supplies the hottest module paths in
// descending request order (store.Store.Popular).
func NewWarmer(p *Proxy, popular func(limit int) []string, log *slog.Logger) *Warmer {
	if log == nil {
		log = slog.Default()
	}
	return &Warmer{
		p:        p,
		popular:  popular,
		interval: PrewarmInterval,
		jitter:   PrewarmJitter,
		log:      log,
	}
}

// Run syncs immediately, then once per jittered interval until ctx is
// cancelled.
func (w *Warmer) Run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		sum := w.Sync(ctx)
		if ctx.Err() != nil {
			return
		}
		w.report(sum)
		timer.Reset(w.next())
	}
}

// Sync runs one prewarm pass with bounded parallelism.
func (w *Warmer) Sync(ctx context.Context) WarmSummary {
	mods := w.popular(PrewarmLimit)
	sum := WarmSummary{Modules: len(mods)}
	if len(mods) == 0 {
		return sum
	}
	var fetched, failed, bytes atomic.Int64
	work := make(chan string)
	var wg sync.WaitGroup
	for range prewarmWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for mod := range work {
				n, ok := w.warmModule(ctx, mod)
				if !ok {
					failed.Add(1)
					continue
				}
				if n > 0 {
					fetched.Add(1)
					bytes.Add(n)
				}
			}
		}()
	}
feed:
	for _, mod := range mods {
		select {
		case <-ctx.Done():
			break feed
		case work <- mod:
		}
	}
	close(work)
	wg.Wait()
	sum.Fetched = int(fetched.Load())
	sum.Bytes = bytes.Load()
	sum.Failed = int(failed.Load())
	return sum
}

// warmModule resolves the latest version of one module and caches its
// files. It returns the bytes pulled and whether the module was handled
// without error.
func (w *Warmer) warmModule(ctx context.Context, mod string) (int64, bool) {
	ctx, cancel := context.WithTimeout(ctx, prewarmTimeout)
	defer cancel()
	ver, err := w.p.latestVersion(ctx, mod)
	if err != nil {
		w.log.Debug("prewarm: resolving latest", "module", mod, "err", err)
		return 0, false
	}
	var bytes int64
	for _, kind := range []store.Kind{store.KindInfo, store.KindMod, store.KindZip} {
		n, err := w.p.warmFile(ctx, mod, ver, kind)
		if err != nil {
			w.log.Debug("prewarm: fetching", "module", mod, "version", ver, "kind", kind, "err", err)
			return bytes, false
		}
		bytes += n
	}
	return bytes, true
}

func (w *Warmer) report(sum WarmSummary) {
	if sum.Modules == 0 {
		return
	}
	w.log.Info("prewarm: fleet cache sync",
		"modules", sum.Modules, "fetched", sum.Fetched, "bytes", sum.Bytes, "failed", sum.Failed)
	w.p.stats.Prewarm(sum.Modules, sum.Fetched, sum.Bytes)
}

// next returns the next interval with symmetric jitter applied.
func (w *Warmer) next() time.Duration {
	if w.jitter <= 0 || w.interval <= 2*w.jitter {
		return w.interval
	}
	return w.interval - w.jitter + time.Duration(rand.Int64N(int64(2*w.jitter)))
}

// warmFile fetches one module file through the shared singleflight path, so
// a prewarm and a client request for the same file never download it twice.
func (p *Proxy) warmFile(ctx context.Context, mod, ver string, kind store.Kind) (int64, error) {
	v, err, _ := p.inflight.Do(negKey(mod, ver, kind), func() (any, error) {
		return p.fetch(ctx, mod, ver, kind)
	})
	if err != nil {
		return 0, err
	}
	res, ok := v.(*fetchResult)
	if !ok || res == nil {
		return 0, nil
	}
	return res.bytesUp, nil
}

// latestVersion asks the upstream which canonical version a module's latest
// tag resolves to. Only the upstream is authoritative for that: a neighbour
// forwards the same question, so asking it would just add a hop.
func (p *Proxy) latestVersion(ctx context.Context, mod string) (string, error) {
	esc, err := module.EscapePath(mod)
	if err != nil {
		return "", err
	}
	u := p.upstream.JoinPath(esc, "@latest")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("proxy: %w", err)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("proxy: latest %s: %w", mod, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", &statusError{
			code: resp.StatusCode,
			body: fmt.Sprintf("upstream %s: %s", u.Host, statusSummary(resp.Body)),
		}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, smallBodyMax+1))
	if err != nil {
		return "", fmt.Errorf("proxy: latest %s: %w", mod, err)
	}
	var probe struct {
		Version string `json:"Version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return "", fmt.Errorf("proxy: latest %s: invalid info json: %w", mod, err)
	}
	if !semver.IsValid(probe.Version) {
		return "", fmt.Errorf("proxy: latest %s: invalid version %q", mod, probe.Version)
	}
	return probe.Version, nil
}
