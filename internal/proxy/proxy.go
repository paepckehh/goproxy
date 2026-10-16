// Package proxy implements the GOPROXY module protocol endpoints:
//
//	GET /<module>/@v/list                  forwarded upstream
//	GET /<module>/@latest                  forwarded upstream
//	GET /<module>/@v/<version>.info        cached, JSON-validated
//	GET /<module>/@v/<version>.mod         cached, hash-verified
//	GET /<module>/@v/<version>.zip         cached, hash-verified
//	GET /sumdb/<database>/...              transparently forwarded
//
// Cache misses are deduplicated with singleflight, downloaded to a temp
// file, verified against the signed checksum database record, then
// committed to the cache with an atomic rename.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	"golang.org/x/sync/singleflight"

	"paepcke.de/goproxy/internal/stats"
	"paepcke.de/goproxy/internal/store"
	"paepcke.de/goproxy/internal/sumdb"
)

const (
	negTTL       = time.Minute
	smallBodyMax = 4 << 20 // .info and .mod bodies must stay small
)

// statusError passes an upstream status code through to the client.
type statusError struct {
	code int
	body string
}

func (e *statusError) Error() string { return fmt.Sprintf("upstream status %d", e.code) }

// verifyError reports failed checksum verification; content is refused.
type verifyError struct{ detail string }

func (e *verifyError) Error() string { return e.detail }

// fetchResult is what a completed download looks like.
type fetchResult struct {
	file       *store.File
	bytesUp    int64
	verified   bool
	unverified bool
	fromCache  bool
}

// Proxy serves the module protocol against a cache and an upstream.
type Proxy struct {
	store    *store.Store
	sumdb    *sumdb.DB
	stats    *stats.Stats
	upstream *url.URL
	verify   bool
	client   *http.Client
	log      *slog.Logger

	inflight singleflight.Group

	negMu sync.Mutex
	neg   map[string]int // upstream 404/410 negative cache, per request key

	sumdbProxy http.Handler
}

// New creates the proxy. upstream is e.g. https://proxy.golang.org.
func New(st *store.Store, db *sumdb.DB, m *stats.Stats, upstream string, verify bool, client *http.Client, log *slog.Logger) (*Proxy, error) {
	u, err := url.Parse(upstream)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("proxy: invalid upstream url %q", upstream)
	}
	if client == nil {
		client = &http.Client{}
	}
	if log == nil {
		log = slog.Default()
	}
	rp := httputil.NewSingleHostReverseProxy(db.Base())
	return &Proxy{
		store:      st,
		sumdb:      db,
		stats:      m,
		upstream:   u,
		verify:     verify,
		client:     client,
		log:        log,
		neg:        make(map[string]int),
		sumdbProxy: rp,
	}, nil
}

// ServeSumDB transparently forwards checksum database requests, so go
// clients can run their own full verification through this proxy.
func (p *Proxy) ServeSumDB(w http.ResponseWriter, r *http.Request) {
	p.sumdbProxy.ServeHTTP(w, r)
}

// ServeModule handles a module protocol request. The path must be the
// request path (starting with /).
func (p *Proxy) ServeModule(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/")

	if mod, ok := strings.CutSuffix(rest, "/@latest"); ok && mod != "" {
		p.passThrough(w, r, mod, "@latest", start)
		return
	}
	mod, tail, ok := strings.Cut(rest, "/@v/")
	if !ok || mod == "" {
		p.badRequest(w, mod, "invalid module request path")
		return
	}
	if tail == "list" {
		p.passThrough(w, r, mod, "@v/list", start)
		return
	}

	dot := strings.LastIndexByte(tail, '.')
	if dot < 0 {
		p.badRequest(w, mod, "missing file kind")
		return
	}
	verEsc, kind := tail[:dot], tail[dot+1:]
	if kind != string(store.KindInfo) && kind != string(store.KindMod) && kind != string(store.KindZip) {
		p.badRequest(w, mod, "unknown file kind "+kind)
		return
	}
	modPath, err := module.UnescapePath(mod)
	if err != nil {
		p.badRequest(w, mod, "invalid escaped module path")
		return
	}
	if err := module.CheckPath(modPath); err != nil {
		p.badRequest(w, modPath, "invalid module path")
		return
	}
	ver, err := module.UnescapeVersion(verEsc)
	if err != nil || !semver.IsValid(ver) {
		p.badRequest(w, modPath, "invalid version "+verEsc)
		return
	}

	p.serveVersioned(w, r, modPath, ver, store.Kind(kind), start)
}

func (p *Proxy) badRequest(w http.ResponseWriter, mod, detail string) {
	p.stats.Error(mod, "request", detail)
	http.Error(w, "goproxy: "+detail, http.StatusBadRequest)
}

// serveVersioned serves a versioned, immutable module file.
func (p *Proxy) serveVersioned(w http.ResponseWriter, r *http.Request, mod, ver string, kind store.Kind, start time.Time) {
	if f, err := p.store.Open(mod, ver, kind); err == nil {
		p.serveFile(w, r, f, mod, ver, kind, start, true)
		return
	}

	key := mod + "@" + ver + ":" + string(kind)
	v, err, shared := p.inflight.Do(key, func() (any, error) {
		return p.fetch(r.Context(), mod, ver, kind)
	})
	if err != nil {
		p.serveFetchError(w, err, mod, ver, kind, start)
		return
	}
	res := v.(*fetchResult)
	if res.fromCache || shared {
		// A concurrent request populated the cache while we queued.
		p.serveFile(w, r, res.file, mod, ver, kind, start, true)
		return
	}
	served := p.serveFile(w, r, res.file, mod, ver, kind, start, false)
	p.stats.Miss(mod, ver, string(kind), res.bytesUp, served, time.Since(start), res.verified)
}

// serveFile streams a cached file and accounts the hit (or the served half
// of a miss). It returns the number of bytes served.
func (p *Proxy) serveFile(w http.ResponseWriter, r *http.Request, f *store.File, mod, ver string, kind store.Kind, start time.Time, wasHit bool) int64 {
	file, err := f.Open()
	if err != nil {
		p.stats.Error(mod, string(kind), "open cached file: "+err.Error())
		http.Error(w, "goproxy: cache file missing", http.StatusInternalServerError)
		return 0
	}
	defer file.Close()

	w.Header().Set("Content-Type", contentType(kind))
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")

	cw := &countingWriter{ResponseWriter: w}
	http.ServeContent(cw, r, f.Path, f.FetchedAt, file)

	n := cw.n
	f.RecordHit(n)
	if wasHit {
		p.stats.Hit(mod, ver, string(kind), n, time.Since(start))
	}
	return n
}

func contentType(kind store.Kind) string {
	switch kind {
	case store.KindInfo:
		return "application/json"
	case store.KindMod:
		return "text/plain; charset=utf-8"
	default:
		return "application/zip"
	}
}

// fetch downloads, verifies and caches a module file.
func (p *Proxy) fetch(ctx context.Context, mod, ver string, kind store.Kind) (*fetchResult, error) {
	if f, err := p.store.Open(mod, ver, kind); err == nil {
		return &fetchResult{file: f, fromCache: true, verified: f.Verified}, nil
	}
	if code := p.negative(mod, ver, kind); code != 0 {
		return nil, &statusError{code: code, body: "upstream previously reported " + http.StatusText(code)}
	}

	esc, err := module.EscapePath(mod)
	if err != nil {
		return nil, &statusError{code: http.StatusBadRequest, body: err.Error()}
	}
	escv, err := module.EscapeVersion(ver)
	if err != nil {
		return nil, &statusError{code: http.StatusBadRequest, body: err.Error()}
	}
	u := p.upstream.JoinPath(esc, "@v", escv+"."+string(kind))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("proxy: %w", err)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("proxy: fetch %s@%s: %w", mod, ver, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
			p.setNegative(mod, ver, kind, resp.StatusCode)
		}
		return nil, &statusError{
			code: resp.StatusCode,
			body: fmt.Sprintf("upstream %s: %s", u.Host, statusSummary(resp.Body)),
		}
	}

	// The signed checksum record for this version, when one exists.
	var rec *sumdb.Record
	if p.verify && kind != store.KindInfo {
		rec, err = p.sumdb.Lookup(mod, ver)
		if errors.Is(err, sumdb.ErrUnknown) {
			// Private or brand-new module: no record, serve unverified.
			rec = nil
			p.log.Warn("proxy: no checksum record, serving unverified",
				"module", mod, "version", ver)
		} else if err != nil {
			return nil, fmt.Errorf("proxy: checksum db lookup %s@%s: %w", mod, ver, err)
		}
	}

	switch kind {
	case store.KindZip:
		return p.fetchZip(resp.Body, mod, ver, rec)
	case store.KindMod:
		return p.fetchMod(resp.Body, mod, ver, rec)
	default:
		return p.fetchInfo(resp.Body, mod, ver)
	}
}

func (p *Proxy) fetchZip(body io.Reader, mod, ver string, rec *sumdb.Record) (*fetchResult, error) {
	tmp, err := p.store.Create()
	if err != nil {
		return nil, fmt.Errorf("proxy: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after a successful commit
	size, err := io.Copy(tmp, body)
	cerr := tmp.Close()
	if err != nil || cerr != nil {
		return nil, errors.Join(err, cerr)
	}

	hash, err := sumdb.HashZip(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("proxy: hash %s@%s: %w", mod, ver, err)
	}
	if rec != nil && strings.TrimPrefix(hash, "h1:") != rec.ZipHash {
		return nil, &verifyError{detail: fmt.Sprintf("zip hash mismatch for %s@%s: got %s, want h1:%s", mod, ver, hash, rec.ZipHash)}
	}
	f, err := p.store.Put(mod, ver, store.KindZip, tmpPath, hash, rec != nil)
	if err != nil {
		return nil, fmt.Errorf("proxy: %w", err)
	}
	if rec != nil {
		p.putRecord(mod, ver, rec)
	}
	return &fetchResult{file: f, bytesUp: size, verified: rec != nil}, nil
}

func (p *Proxy) fetchMod(body io.Reader, mod, ver string, rec *sumdb.Record) (*fetchResult, error) {
	data, err := io.ReadAll(io.LimitReader(body, smallBodyMax+1))
	if err != nil {
		return nil, fmt.Errorf("proxy: read %s@%s: %w", mod, ver, err)
	}
	if len(data) > smallBodyMax {
		return nil, fmt.Errorf("proxy: %s@%s: go.mod exceeds %d bytes", mod, ver, smallBodyMax)
	}
	hash, err := sumdb.HashMod(data)
	if err != nil {
		return nil, fmt.Errorf("proxy: hash %s@%s: %w", mod, ver, err)
	}
	if rec != nil && strings.TrimPrefix(hash, "h1:") != rec.ModHash {
		return nil, &verifyError{detail: fmt.Sprintf("go.mod hash mismatch for %s@%s: got %s, want h1:%s", mod, ver, hash, rec.ModHash)}
	}
	f, err := p.store.PutBytes(mod, ver, store.KindMod, data, hash, rec != nil)
	if err != nil {
		return nil, fmt.Errorf("proxy: %w", err)
	}
	if rec != nil {
		p.putRecord(mod, ver, rec)
	}
	return &fetchResult{file: f, bytesUp: int64(len(data)), verified: rec != nil}, nil
}

func (p *Proxy) fetchInfo(body io.Reader, mod, ver string) (*fetchResult, error) {
	data, err := io.ReadAll(io.LimitReader(body, smallBodyMax+1))
	if err != nil {
		return nil, fmt.Errorf("proxy: read %s@%s: %w", mod, ver, err)
	}
	if len(data) > smallBodyMax {
		return nil, fmt.Errorf("proxy: %s@%s: info exceeds %d bytes", mod, ver, smallBodyMax)
	}
	var probe struct {
		Version string `json:"Version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("proxy: %s@%s: invalid info json: %w", mod, ver, err)
	}
	f, err := p.store.PutBytes(mod, ver, store.KindInfo, data, "", false)
	if err != nil {
		return nil, fmt.Errorf("proxy: %w", err)
	}
	return &fetchResult{file: f, bytesUp: int64(len(data))}, nil
}

func (p *Proxy) putRecord(mod, ver string, rec *sumdb.Record) {
	if err := p.store.PutRecord(mod, ver, rec.Raw); err != nil {
		p.log.Warn("proxy: storing checksum record", "module", mod, "version", ver, "err", err)
	}
}

// passThrough forwards a mutable request (list, latest) without caching.
func (p *Proxy) passThrough(w http.ResponseWriter, r *http.Request, mod, what string, start time.Time) {
	modPath, err := module.UnescapePath(mod)
	if err != nil {
		p.badRequest(w, mod, "invalid escaped module path")
		return
	}
	if err := module.CheckPath(modPath); err != nil {
		p.badRequest(w, modPath, "invalid module path")
		return
	}
	esc, err := module.EscapePath(modPath)
	if err != nil {
		p.badRequest(w, modPath, "invalid module path")
		return
	}
	u := p.upstream.JoinPath(esc, what)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		p.stats.Error(modPath, what, err.Error())
		http.Error(w, "goproxy: bad request", http.StatusBadRequest)
		return
	}
	resp, err := p.client.Do(req)
	if err != nil {
		p.stats.Error(modPath, what, err.Error())
		http.Error(w, "goproxy: upstream unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Cache-Control", "no-store")
	if resp.StatusCode != http.StatusOK {
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		p.stats.UpstreamStatus(modPath, "", what, resp.StatusCode, time.Since(start))
		return
	}
	cw := &countingWriter{ResponseWriter: w}
	w.WriteHeader(http.StatusOK)
	io.Copy(cw, resp.Body)
	p.stats.Pass(modPath, what, cw.n, time.Since(start))
}

func (p *Proxy) serveFetchError(w http.ResponseWriter, err error, mod, ver string, kind store.Kind, start time.Time) {
	var se *statusError
	if errors.As(err, &se) {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "goproxy: "+se.body, se.code)
		p.stats.UpstreamStatus(mod, ver, string(kind), se.code, time.Since(start))
		return
	}
	var ve *verifyError
	if errors.As(err, &ve) {
		p.log.Error("proxy: refused tampered module",
			"module", mod, "version", ver, "kind", kind, "err", ve.detail)
		http.Error(w, "goproxy: "+ve.detail, http.StatusBadGateway)
		p.stats.VerifyFailure(mod, ver, ve.detail)
		return
	}
	p.log.Error("proxy: fetch failed", "module", mod, "version", ver, "kind", kind, "err", err)
	http.Error(w, "goproxy: "+err.Error(), http.StatusBadGateway)
	p.stats.Error(mod, string(kind), err.Error())
}

func statusSummary(body io.Reader) string {
	data, err := io.ReadAll(io.LimitReader(body, 256))
	if err != nil || len(data) == 0 {
		return http.StatusText(http.StatusBadGateway)
	}
	return strings.TrimSpace(string(data))
}

// negative returns the cached negative upstream status for a key, if fresh.
func (p *Proxy) negative(mod, ver string, kind store.Kind) int {
	p.negMu.Lock()
	defer p.negMu.Unlock()
	code, ok := p.neg[negKey(mod, ver, kind)]
	if !ok {
		return 0
	}
	return code
}

func (p *Proxy) setNegative(mod, ver string, kind store.Kind, code int) {
	p.negMu.Lock()
	defer p.negMu.Unlock()
	// ponytail: unbounded growth guard; a full reset just refetches.
	if len(p.neg) > 10000 {
		p.neg = make(map[string]int)
	}
	p.neg[negKey(mod, ver, kind)] = code
	time.AfterFunc(negTTL, func() {
		p.negMu.Lock()
		delete(p.neg, negKey(mod, ver, kind))
		p.negMu.Unlock()
	})
}

func negKey(mod, ver string, kind store.Kind) string {
	return mod + "@" + ver + ":" + string(kind)
}

type countingWriter struct {
	http.ResponseWriter
	n int64
}

func (c *countingWriter) Write(b []byte) (int, error) {
	n, err := c.ResponseWriter.Write(b)
	c.n += int64(n)
	return n, err
}
