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

	"paepcke.de/goproxy/internal/fleet"
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

	mu   sync.RWMutex
	last fleet.WarmReport
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

// Report returns the most recent sync, or the zero report before the first
// pass. The item slice is a copy.
func (w *Warmer) Report() fleet.WarmReport {
	w.mu.RLock()
	defer w.mu.RUnlock()
	out := w.last
	out.Items = append([]fleet.WarmItem(nil), w.last.Items...)
	return out
}

func (w *Warmer) store(report fleet.WarmReport) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.last = report
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
		report := w.Sync(ctx)
		if ctx.Err() != nil {
			return
		}
		next := w.next()
		report.NextRun = time.Now().UTC().Add(next)
		w.store(report)
		w.report(report)
		timer.Reset(next)
	}
}

// Sync runs one prewarm pass with bounded parallelism and records the
// report for the dashboard.
func (w *Warmer) Sync(ctx context.Context) fleet.WarmReport {
	start := time.Now()
	mods := w.popular(PrewarmLimit)
	report := fleet.WarmReport{StartedAt: start.UTC()}
	if len(mods) == 0 {
		report.DurationMS = time.Since(start).Milliseconds()
		w.store(report)
		return report
	}

	items := make([]fleet.WarmItem, len(mods))
	type job struct {
		idx int
		mod string
	}
	work := make(chan job)
	var wg sync.WaitGroup
	for range prewarmWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range work {
				items[j.idx] = w.warmModule(ctx, j.mod)
			}
		}()
	}
	attempted := 0
feed:
	for i, mod := range mods {
		select {
		case <-ctx.Done():
			break feed
		case work <- job{idx: i, mod: mod}:
			attempted++
		}
	}
	close(work)
	wg.Wait()

	var fetched, failed atomic.Int64
	var bytes atomic.Int64
	for _, it := range items[:attempted] {
		if it.Err != "" {
			failed.Add(1)
			continue
		}
		if it.Bytes > 0 {
			fetched.Add(1)
			bytes.Add(it.Bytes)
		}
	}
	report.Modules = attempted
	report.Fetched = int(fetched.Load())
	report.Bytes = bytes.Load()
	report.Failed = int(failed.Load())
	report.Items = items[:attempted]
	report.DurationMS = time.Since(start).Milliseconds()
	w.store(report)
	return report
}

// warmModule resolves the latest version of one module and caches its
// files. The returned item reports the bytes pulled, the peer that served
// them and, on failure, what went wrong.
func (w *Warmer) warmModule(ctx context.Context, mod string) fleet.WarmItem {
	item := fleet.WarmItem{Module: mod}
	ctx, cancel := context.WithTimeout(ctx, prewarmTimeout)
	defer cancel()
	ver, err := w.p.latestVersion(ctx, mod)
	if err != nil {
		w.log.Debug("prewarm: resolving latest", "module", mod, "err", err)
		item.Err = err.Error()
		return item
	}
	item.Version = ver
	for _, kind := range []store.Kind{store.KindInfo, store.KindMod, store.KindZip} {
		res, err := w.p.warmFile(ctx, mod, ver, kind)
		if err != nil {
			w.log.Debug("prewarm: fetching", "module", mod, "version", ver, "kind", kind, "err", err)
			item.Err = err.Error()
			return item
		}
		item.Bytes += res.bytesUp
		if res.neighbour != "" {
			item.Source = res.neighbour
		}
	}
	return item
}

func (w *Warmer) report(r fleet.WarmReport) {
	if r.Modules == 0 {
		return
	}
	w.log.Info("prewarm: fleet cache sync",
		"modules", r.Modules, "fetched", r.Fetched, "bytes", r.Bytes, "failed", r.Failed)
	w.p.stats.Prewarm(r.Modules, r.Fetched, r.Failed, r.Bytes)
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
func (p *Proxy) warmFile(ctx context.Context, mod, ver string, kind store.Kind) (*fetchResult, error) {
	v, err, _ := p.inflight.Do(negKey(mod, ver, kind), func() (any, error) {
		return p.fetch(ctx, mod, ver, kind)
	})
	if err != nil {
		return nil, err
	}
	res, ok := v.(*fetchResult)
	if !ok || res == nil {
		return &fetchResult{}, nil
	}
	return res, nil
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
