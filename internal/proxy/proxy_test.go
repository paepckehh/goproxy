package proxy

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	xsumdb "golang.org/x/mod/sumdb"
	"golang.org/x/mod/sumdb/note"

	"paepcke.de/goproxy/internal/stats"
	"paepcke.de/goproxy/internal/store"
	"paepcke.de/goproxy/internal/sumdb"
)

const (
	testMod  = "example.com/m"
	testVer  = "v1.0.0"
	testVer2 = "v1.1.0"
)

type testEnv struct {
	proxy   *Proxy
	store   *store.Store
	stats   *stats.Stats
	sumdbDB *sumdb.DB

	upstreamHits *atomic.Int64
	upstreamURL  string
}

func buildModuleZip(t *testing.T, mod, ver string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(mod + "@" + ver + "/go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("module " + mod + "\n\ngo 1.21\n")); err != nil {
		t.Fatal(err)
	}
	w, err = zw.Create(mod + "@" + ver + "/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("package main\n\nfunc main() {}\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newTestEnv(t *testing.T, gosum func(path, ver string) ([]byte, error)) *testEnv {
	t.Helper()
	modBytes := []byte("module " + testMod + "\n\ngo 1.21\n")
	zipBytes := buildModuleZip(t, testMod, testVer)

	zipPath := filepath.Join(t.TempDir(), "m.zip")
	if err := os.WriteFile(zipPath, zipBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	modHash, err := sumdb.HashMod(modBytes)
	if err != nil {
		t.Fatal(err)
	}
	zipHash, err := sumdb.HashZip(zipPath)
	if err != nil {
		t.Fatal(err)
	}

	var upstreamHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		switch r.URL.Path {
		case "/" + testMod + "/@v/list":
			fmt.Fprint(w, testVer+"\n"+testVer2+"\n")
		case "/" + testMod + "/@latest":
			fmt.Fprint(w, `{"Version":"`+testVer2+`","Time":"2026-01-01T00:00:00Z"}`)
		case "/" + testMod + "/@v/" + testVer + ".info":
			fmt.Fprint(w, `{"Version":"`+testVer+`","Time":"2025-01-01T00:00:00Z"}`)
		case "/" + testMod + "/@v/" + testVer + ".mod":
			w.Write(modBytes)
		case "/" + testMod + "/@v/" + testVer + ".zip":
			w.Write(zipBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	if gosum == nil {
		gosum = func(path, ver string) ([]byte, error) {
			if path != testMod {
				return nil, errors.New("unknown module")
			}
			return []byte(path + " " + ver + " " + zipHash + "\n" +
				path + " " + ver + "/go.mod " + modHash + "\n"), nil
		}
	}
	skey, vkey, err := note.GenerateKey(rand.Reader, "test.sumdb.example")
	if err != nil {
		t.Fatal(err)
	}
	sumSrv := httptest.NewServer(xsumdb.NewServer(xsumdb.NewTestServer(skey, gosum)))
	t.Cleanup(sumSrv.Close)

	root := t.TempDir()
	st, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sumdb.New(sumSrv.URL, vkey, filepath.Join(root, "sumdb-state"), sumSrv.Client(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	m := stats.New(nil)
	p, err := New(st, db, m, upstream.URL, true, upstream.Client(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return &testEnv{proxy: p, store: st, stats: m, sumdbDB: db,
		upstreamHits: &upstreamHits, upstreamURL: upstream.URL}
}

func (e *testEnv) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	e.proxy.ServeModule(rec, req)
	return rec
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for range 500 {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not reached in time")
}

func TestProxy_ModFetchVerifyCache(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)

	rec := env.get(t, "/"+testMod+"/@v/"+testVer+".mod")
	if rec.Code != http.StatusOK {
		t.Fatalf("mod status = %d, body %q", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "module "+testMod+"\n\ngo 1.21\n" {
		t.Errorf("mod body = %q", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("mod content type = %q", ct)
	}
	if f := env.store.Get(testMod, testVer, store.KindMod); f == nil || !f.Verified {
		t.Fatal("mod not cached and verified")
	}
	if _, err := os.Stat(filepath.Join(env.store.Root(), "download", testMod, "@v", testVer+".sum")); err != nil {
		t.Errorf("signed record sidecar missing: %v", err)
	}

	// Second request is a hit served from cache without touching upstream.
	hitsBefore := env.upstreamHits.Load()
	rec = env.get(t, "/"+testMod+"/@v/"+testVer+".mod")
	if rec.Code != http.StatusOK {
		t.Fatalf("second mod status = %d", rec.Code)
	}
	if env.upstreamHits.Load() != hitsBefore {
		t.Error("cache hit still reached upstream")
	}

	waitFor(t, func() bool { return env.stats.Snapshot().Lifetime.Misses >= 1 })
	tot := env.stats.Snapshot().Lifetime
	if tot.Hits != 1 || tot.Misses != 1 || tot.Verifications != 1 {
		t.Errorf("totals = %+v, want 1 hit, 1 verified miss", tot)
	}
}

func TestProxy_ZipRoundtrip(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)

	zipBytes := buildModuleZip(t, testMod, testVer)
	rec := env.get(t, "/"+testMod+"/@v/"+testVer+".zip")
	if rec.Code != http.StatusOK {
		t.Fatalf("zip status = %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), zipBytes) {
		t.Error("zip body differs from upstream content")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Errorf("zip content type = %q", ct)
	}
	if f := env.store.Get(testMod, testVer, store.KindZip); f == nil || !f.Verified {
		t.Fatal("zip not cached and verified")
	}
}

func TestProxy_InfoVersionValidated(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body string
	}{
		{"wrong version", `{"Version":"v9.9.9","Time":"2024-01-01T00:00:00Z"}`},
		{"null", `null`},
		{"empty object", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newTestEnv(t, nil)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, tc.body)
			}))
			t.Cleanup(upstream.Close)

			p, err := New(env.store, env.sumdbDB, env.stats, upstream.URL, false, upstream.Client(), slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "/"+testMod+"/@v/"+testVer+".info", nil)
			rec := httptest.NewRecorder()
			p.ServeModule(rec, req)
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502 for invalid info body, body %q", rec.Code, rec.Body.String())
			}
			if env.store.Get(testMod, testVer, store.KindInfo) != nil {
				t.Fatal("invalid .info response was cached")
			}
		})
	}
}

func TestProxy_InfoRoundtrip(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)

	rec := env.get(t, "/"+testMod+"/@v/"+testVer+".info")
	if rec.Code != http.StatusOK {
		t.Fatalf("info status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"Version":"`+testVer+`"`) {
		t.Errorf("info body = %q", rec.Body.String())
	}
	if f := env.store.Get(testMod, testVer, store.KindInfo); f == nil {
		t.Fatal("info not cached")
	}
}

func TestProxy_VerifyFailureRefusesContent(t *testing.T) {
	t.Parallel()
	// The database promises hashes of content the upstream will NOT send.
	env := newTestEnv(t, func(path, ver string) ([]byte, error) {
		return []byte(path + " " + ver + " h1:WRONGHASH=\n" +
			path + " " + ver + "/go.mod h1:WRONGMOD=\n"), nil
	})

	rec := env.get(t, "/"+testMod+"/@v/"+testVer+".mod")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 on hash mismatch", rec.Code)
	}
	if env.store.Get(testMod, testVer, store.KindMod) != nil {
		t.Error("tampered module was cached")
	}
	waitFor(t, func() bool { return env.stats.Snapshot().Lifetime.VerifyFailures >= 1 })

	// The zip is also refused.
	rec = env.get(t, "/"+testMod+"/@v/"+testVer+".zip")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("zip status = %d, want 502", rec.Code)
	}
}

func TestProxy_PartialRecordVerifiesWhatItCan(t *testing.T) {
	t.Parallel()
	// A database record with only a zip line (custom databases): the zip
	// is hash-verified, the go.mod is served unverified, nothing refused.
	zipPath := filepath.Join(t.TempDir(), "m.zip")
	if err := os.WriteFile(zipPath, buildModuleZip(t, testMod, testVer), 0o644); err != nil {
		t.Fatal(err)
	}
	zipHash, err := sumdb.HashZip(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	env := newTestEnv(t, func(path, ver string) ([]byte, error) {
		return []byte(path + " " + strings.TrimSuffix(ver, "/go.mod") + " " + zipHash + "\n"), nil
	})

	rec := env.get(t, "/"+testMod+"/@v/"+testVer+".mod")
	if rec.Code != http.StatusOK {
		t.Fatalf("mod status = %d, want 200: no go.mod hash line is not tampering", rec.Code)
	}
	if f := env.store.Get(testMod, testVer, store.KindMod); f == nil || f.Verified {
		t.Fatal("go.mod cached as verified without a signed go.mod hash")
	}

	rec = env.get(t, "/"+testMod+"/@v/"+testVer+".zip")
	if rec.Code != http.StatusOK {
		t.Fatalf("zip status = %d, want 200", rec.Code)
	}
	if f := env.store.Get(testMod, testVer, store.KindZip); f == nil || !f.Verified {
		t.Fatal("zip with matching signed hash not verified")
	}
}

func TestProxy_ListAndLatestPassThrough(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)

	rec := env.get(t, "/"+testMod+"/@v/list")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), testVer2) {
		t.Errorf("list: status %d body %q", rec.Code, rec.Body.String())
	}
	rec = env.get(t, "/"+testMod+"/@latest")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), testVer2) {
		t.Errorf("latest: status %d body %q", rec.Code, rec.Body.String())
	}
	// Mutable endpoints must not be cached.
	if env.store.Get(testMod, "", store.KindInfo) != nil {
		t.Error("list/latest polluted the store")
	}
}

func TestProxy_UpstreamNotFoundAndNegativeCache(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)

	rec := env.get(t, "/example.com/none/@v/v9.9.9.mod")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	hits := env.upstreamHits.Load()

	// Second miss for the same key is served from the negative cache.
	rec = env.get(t, "/example.com/none/@v/v9.9.9.mod")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second status = %d, want 404", rec.Code)
	}
	if env.upstreamHits.Load() != hits {
		t.Error("negative cache did not suppress upstream request")
	}
}

func TestProxy_NegativeCacheExpires(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	env.proxy.setNegative(testMod, testVer, store.KindMod, http.StatusNotFound)
	if got := env.proxy.negative(testMod, testVer, store.KindMod); got != http.StatusNotFound {
		t.Fatalf("negative status = %d, want 404", got)
	}

	key := negKey(testMod, testVer, store.KindMod)
	env.proxy.negMu.Lock()
	env.proxy.neg[key] = negEntry{code: http.StatusNotFound, expires: time.Now().Add(-time.Second)}
	env.proxy.negMu.Unlock()
	if got := env.proxy.negative(testMod, testVer, store.KindMod); got != 0 {
		t.Fatalf("expired negative status = %d, want a miss", got)
	}
	env.proxy.negMu.Lock()
	_, present := env.proxy.neg[key]
	env.proxy.negMu.Unlock()
	if present {
		t.Fatal("expired negative entry was not removed")
	}
}

func TestProxy_UnknownToChecksumDBServesUnverified(t *testing.T) {
	t.Parallel()
	// A checksum database that 404s every lookup, like sum.golang.org for
	// unknown (e.g. private) modules.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	root := t.TempDir()
	st, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sumdb.New(srv.URL, sumdb.DefaultKey, filepath.Join(root, "sumdb-state"), srv.Client(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "module example.com/private\n")
	}))
	t.Cleanup(upstream.Close)

	m := stats.New(nil)
	p, err := New(st, db, m, upstream.URL, true, upstream.Client(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	env := &testEnv{proxy: p, store: st, stats: m, upstreamHits: &atomic.Int64{}}

	rec := env.get(t, "/example.com/private/@v/v1.0.0.mod")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for unknown module", rec.Code)
	}
	f := env.store.Get("example.com/private", "v1.0.0", store.KindMod)
	if f == nil || f.Verified {
		t.Fatal("module not cached, or wrongly marked verified")
	}
}

func TestProxy_BadRequests(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)

	cases := []struct {
		name string
		path string
		code int
	}{
		{"traversal module", "/../../../etc/@v/v1.0.0.mod", http.StatusBadRequest},
		{"bad version", "/" + testMod + "/@v/latest.mod", http.StatusBadRequest},
		{"bad kind", "/" + testMod + "/@v/v1.0.0.exe", http.StatusBadRequest},
		{"no atv", "/" + testMod + "/v1.0.0.mod", http.StatusBadRequest},
		{"empty module", "/@v/list", http.StatusBadRequest},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if rec := env.get(t, tt.path); rec.Code != tt.code {
				t.Errorf("path %q status = %d, want %d", tt.path, rec.Code, tt.code)
			}
		})
	}
}

func TestProxy_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/"+testMod+"/@v/"+testVer+".mod", nil)
	rec := httptest.NewRecorder()
	env.proxy.ServeModule(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestProxy_EscapedModulePath(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/example.com/!upper/@v/v1.0.0.mod" {
			fmt.Fprint(w, "module example.com/Upper\n")
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(upstream.Close)
	root := t.TempDir()
	st, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	skey, vkey, err := note.GenerateKey(rand.Reader, "test.sumdb.example")
	if err != nil {
		t.Fatal(err)
	}
	sumSrv := httptest.NewServer(xsumdb.NewServer(xsumdb.NewTestServer(skey, func(path, ver string) ([]byte, error) {
		modHash, err := sumdb.HashMod([]byte("module example.com/Upper\n"))
		if err != nil {
			t.Fatal(err)
		}
		return []byte(path + " " + ver + " h1:x=\n" +
			path + " " + ver + "/go.mod " + modHash + "\n"), nil
	})))
	t.Cleanup(sumSrv.Close)
	db, err := sumdb.New(sumSrv.URL, vkey, filepath.Join(root, "sumdb-state"), sumSrv.Client(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	m := stats.New(nil)
	p, err := New(st, db, m, upstream.URL, true, upstream.Client(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/example.com/!upper/@v/v1.0.0.mod", nil)
	rec := httptest.NewRecorder()
	p.ServeModule(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	f := st.Get("example.com/Upper", "v1.0.0", store.KindMod)
	if f == nil || !f.Verified {
		t.Fatal("uppercase module path not cached/verified under its plain name")
	}
}

func TestProxy_SingleflightDeduplicates(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)

	// Slow the upstream down so concurrent requests overlap.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.upstreamHits.Add(1)
		time.Sleep(80 * time.Millisecond)
		if strings.HasSuffix(r.URL.Path, ".zip") {
			w.Write(buildModuleZip(t, testMod, testVer))
			return
		}
		fmt.Fprint(w, "module "+testMod+"\n\ngo 1.21\n")
	}))
	t.Cleanup(slow.Close)

	p, err := New(env.store, env.sumdbDB, env.stats, slow.URL, false, slow.Client(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	const n = 16
	srv := httptest.NewServer(http.HandlerFunc(p.ServeModule))
	t.Cleanup(srv.Close)

	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := srv.Client().Get(srv.URL + "/" + testMod + "/@v/" + testVer + ".zip")
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			if _, err := io.Copy(io.Discard, resp.Body); err != nil {
				t.Error(err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d", resp.StatusCode)
			}
		}()
	}
	wg.Wait()

	if got := env.upstreamHits.Load(); got != 1 {
		t.Errorf("upstream saw %d requests, want 1 (singleflight)", got)
	}

	// The executing request accounts the one miss; the queued ones are hits.
	tot := env.stats.Snapshot().Lifetime
	if tot.Misses != 1 || tot.Hits != n-1 {
		t.Errorf("totals: misses = %d, hits = %d, want 1, %d", tot.Misses, tot.Hits, n-1)
	}
}

func TestProxy_ClientDisconnectDoesNotAbortFetch(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)

	// A slow upstream, so the client can walk away mid-download.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.upstreamHits.Add(1)
		time.Sleep(150 * time.Millisecond)
		fmt.Fprint(w, "module "+testMod+"\n\ngo 1.21\n")
	}))
	t.Cleanup(slow.Close)

	p, err := New(env.store, env.sumdbDB, env.stats, slow.URL, false, slow.Client(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(p.ServeModule))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/"+testMod+"/@v/"+testVer+".mod", nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		resp, err := srv.Client().Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()

	// The shared fetch must finish and warm the cache despite the cancel.
	waitFor(t, func() bool { return env.store.Get(testMod, testVer, store.KindMod) != nil })

	hits := env.upstreamHits.Load()
	req2 := httptest.NewRequest(http.MethodGet, "/"+testMod+"/@v/"+testVer+".mod", nil)
	rec := httptest.NewRecorder()
	p.ServeModule(rec, req2)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the warmed cache", rec.Code)
	}
	if env.upstreamHits.Load() != hits {
		t.Error("a disconnecting client aborted the shared fetch")
	}
}

func TestProxy_SumDBForwarded(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)

	// The go protocol: <proxy>/sumdb/<db>/<path> forwards <path> verbatim
	// to the checksum database <db>.
	req := httptest.NewRequest(http.MethodGet, "/sumdb/test.sumdb.example/latest", nil)
	rec := httptest.NewRecorder()
	env.proxy.ServeSumDB(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("latest: status %d, body %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "test.sumdb.example") {
		t.Errorf("latest body = %q, want the signed tree head", rec.Body.String())
	}
	if passes := env.stats.Snapshot().Lifetime.Passes; passes == 0 {
		t.Error("sumdb forwarding was not counted in the stats")
	}

	// The go client only uses a proxy for checksum-database traffic when
	// this probe answers 200; forwarding it upstream would yield 404.
	req = httptest.NewRequest(http.MethodGet, "/sumdb/test.sumdb.example/supported", nil)
	rec = httptest.NewRecorder()
	env.proxy.ServeSumDB(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("supported: status %d, want 200", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/sumdb/test.sumdb.example/lookup/"+testMod+"@"+testVer, nil)
	rec = httptest.NewRecorder()
	env.proxy.ServeSumDB(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), testMod) {
		t.Errorf("lookup: status %d, body %q", rec.Code, rec.Body.String())
	}

	// Any other database name must not be forwarded.
	req = httptest.NewRequest(http.MethodGet, "/sumdb/evil.example/latest", nil)
	rec = httptest.NewRecorder()
	env.proxy.ServeSumDB(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("foreign db name: status %d, want 404", rec.Code)
	}
}

func TestProxy_SumDBNameWithPath(t *testing.T) {
	t.Parallel()
	const name = "test.sumdb.example/sub"
	skey, vkey, err := note.GenerateKey(rand.Reader, name)
	if err != nil {
		t.Fatal(err)
	}
	sumSrv := httptest.NewServer(xsumdb.NewServer(xsumdb.NewTestServer(skey, func(path, ver string) ([]byte, error) {
		return []byte(path + " " + ver + " h1:zip=\n"), nil
	})))
	t.Cleanup(sumSrv.Close)

	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db, err := sumdb.New(sumSrv.URL, vkey, filepath.Join(t.TempDir(), "state"), sumSrv.Client(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(st, db, stats.New(nil), "http://upstream.invalid", false, sumSrv.Client(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/sumdb/"+name+"/latest", nil)
	rec := httptest.NewRecorder()
	p.ServeSumDB(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), name) {
		t.Fatalf("status %d, body %q; want the signed tree head for %s", rec.Code, rec.Body.String(), name)
	}
}

// fakeReadFromWriter implements io.ReaderFrom so the countingWriter's
// optimized path can be exercised without a real TCP connection.
type fakeReadFromWriter struct {
	header http.Header
	body   bytes.Buffer
}

func (w *fakeReadFromWriter) Header() http.Header { return w.header }
func (w *fakeReadFromWriter) Write(p []byte) (int, error) {
	return w.body.Write(p)
}
func (w *fakeReadFromWriter) WriteHeader(int) {}
func (w *fakeReadFromWriter) ReadFrom(r io.Reader) (int64, error) {
	return io.Copy(&w.body, r)
}

func TestCountingWriterReadFrom(t *testing.T) {
	t.Parallel()
	w := &fakeReadFromWriter{header: make(http.Header)}
	cw := &countingWriter{ResponseWriter: w}
	n, err := io.Copy(cw, strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 || cw.n != 5 || w.body.String() != "hello" {
		t.Fatalf("copy = %d, counted %d, body %q; want 5, 5, hello", n, cw.n, w.body.String())
	}
}

func TestNewRejectsBadUpstream(t *testing.T) {
	t.Parallel()
	for _, upstream := range []string{"", "not a url", "ftp://x"} {
		if _, err := New(nil, nil, nil, upstream, true, nil, nil); err == nil {
			t.Errorf("New(%q) succeeded, want error", upstream)
		}
	}
}

func TestProxy_UpstreamUnreachableFailsClosed(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	// Point the proxy at a dead upstream but a live checksum database.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("should not be reached")
	}))
	url := dead.URL
	dead.Close()

	var logs bytes.Buffer
	p, err := New(env.store, env.sumdbDB, env.stats, url, true, &http.Client{Timeout: 2 * time.Second}, slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/"+testMod+"/@v/"+testVer+".mod", nil)
	rec := httptest.NewRecorder()
	p.ServeModule(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	// The client sees why, and the operator sees it in stderr/journal.
	if body := rec.Body.String(); !strings.Contains(body, "upstream") {
		t.Errorf("body = %q, want the upstream failure reason", body)
	}
	if got := logs.String(); !strings.Contains(got, "fetch failed") || !strings.Contains(got, testMod) {
		t.Errorf("logs = %q, want the upstream failure logged with the module", got)
	}
}

func TestProxy_UpstreamUnreachablePassthroughLogged(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := dead.URL
	dead.Close()

	var logs bytes.Buffer
	p, err := New(env.store, env.sumdbDB, env.stats, url, true, &http.Client{Timeout: 2 * time.Second}, slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/"+testMod+"/@v/list", nil)
	rec := httptest.NewRecorder()
	p.ServeModule(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "upstream unreachable:") {
		t.Errorf("body = %q, want the dial failure detail", body)
	}
	if got := logs.String(); !strings.Contains(got, "upstream unreachable") || !strings.Contains(got, testMod) {
		t.Errorf("logs = %q, want the upstream failure logged with the module", got)
	}
}

func TestProxy_UpstreamErrorStatusLogged(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream exploded", http.StatusInternalServerError)
	}))
	t.Cleanup(broken.Close)

	var logs bytes.Buffer
	p, err := New(env.store, env.sumdbDB, env.stats, broken.URL, true, broken.Client(), slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/"+testMod+"/@v/list", nil)
	rec := httptest.NewRecorder()
	p.ServeModule(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "upstream exploded") {
		t.Errorf("body = %q, want the upstream body passed through", body)
	}
	got := logs.String()
	if !strings.Contains(got, "upstream refused request") || !strings.Contains(got, "upstream exploded") {
		t.Errorf("logs = %q, want the upstream status and body logged", got)
	}
}

// Routine per-request failures must stay out of the journal at the default
// level; DEBUG=1 (LevelDebug) is what surfaces them.
func TestProxy_RoutineFailuresSilentAtDefaultLevel(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream exploded", http.StatusInternalServerError)
	}))
	t.Cleanup(broken.Close)

	var logs bytes.Buffer
	p, err := New(env.store, env.sumdbDB, env.stats, broken.URL, true, broken.Client(),
		slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/"+testMod+"/@v/list", nil)
	rec := httptest.NewRecorder()
	p.ServeModule(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if got := logs.String(); got != "" {
		t.Errorf("logs = %q, want no per-request noise without DEBUG=1", got)
	}
}
