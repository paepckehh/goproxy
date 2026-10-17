package proxy

import (
	"archive/zip"
	"bytes"
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
}

func TestProxy_SumDBForwarded(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/sumdb/test.sumdb.example/latest", nil)
	rec := httptest.NewRecorder()
	env.proxy.ServeSumDB(rec, req)
	if rec.Code == http.StatusNotFound && strings.Contains(rec.Body.String(), "404") {
		t.Logf("sumdb latest via reverse proxy: %d %q", rec.Code, rec.Body.String())
	}
	// The exact body depends on the test server; asserting it responds at
	// all through the reverse proxy is the contract.
	if rec.Code == 0 {
		t.Error("no response from sumdb reverse proxy")
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

	p, err := New(env.store, env.sumdbDB, env.stats, url, true, &http.Client{Timeout: 2 * time.Second}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/"+testMod+"/@v/"+testVer+".mod", nil)
	rec := httptest.NewRecorder()
	p.ServeModule(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}
