package proxy

import (
	"archive/zip"
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"paepcke.de/goproxy/internal/store"
)

// neighbourEnv builds a proxy whose neighbour list points at servers and
// whose upstream is the regular test upstream (hit counter included).
func neighbourEnv(t *testing.T, urls ...string) *Proxy {
	t.Helper()
	env := newTestEnv(t, nil)
	p, err := New(env.store, env.sumdbDB, env.stats, env.upstreamURL, urls, true,
		env.proxy.client, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// peerServer serves module files like a warm neighbouring goproxy.
func peerServer(t *testing.T, modBytes, zipBytes []byte, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		switch {
		case modBytes != nil && r.URL.Path == "/"+testMod+"/@v/"+testVer+".info":
			fmt.Fprint(w, `{"Version":"`+testVer+`","Time":"2025-01-01T00:00:00Z"}`)
		case modBytes != nil && r.URL.Path == "/"+testMod+"/@v/"+testVer+".mod":
			w.Write(modBytes)
		case zipBytes != nil && r.URL.Path == "/"+testMod+"/@v/"+testVer+".zip":
			w.Write(zipBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestProxy_NeighbourServesCacheMiss(t *testing.T) {
	t.Parallel()
	modBytes := []byte("module " + testMod + "\n\ngo 1.21\n")
	zipBytes := buildModuleZip(t, testMod, testVer)

	var peerHits atomic.Int64
	peer := peerServer(t, modBytes, zipBytes, &peerHits)
	p := neighbourEnv(t, peer.URL)

	rec := httptest.NewRecorder()
	p.ServeModule(rec, httptest.NewRequest(http.MethodGet, "/"+testMod+"/@v/"+testVer+".mod", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != string(modBytes) {
		t.Errorf("body = %q", rec.Body.String())
	}
	if f := p.store.Get(testMod, testVer, store.KindMod); f == nil || !f.Verified {
		t.Fatal("neighbour content not cached and verified")
	}
	if peerHits.Load() == 0 {
		t.Fatal("neighbour was never asked")
	}

	waitFor(t, func() bool { return p.stats.Snapshot().Lifetime.NeighbourHits == 1 })
	tot := p.stats.Snapshot().Lifetime
	if tot.Misses != 1 {
		t.Errorf("misses = %d, want 1 (local cache was cold)", tot.Misses)
	}
	if tot.BytesNeighbour != int64(len(modBytes)) {
		t.Errorf("bytesNeighbour = %d, want %d", tot.BytesNeighbour, len(modBytes))
	}
	if tot.BytesUp != 0 {
		t.Errorf("bytesUp = %d, want 0: the upstream was not used", tot.BytesUp)
	}
	if tot.NeighbourMisses != 0 {
		t.Errorf("neighbour misses = %d, want 0", tot.NeighbourMisses)
	}
	if tot.Verifications != 1 {
		t.Errorf("verifications = %d, want 1: neighbour content is verified too", tot.Verifications)
	}
	// The next request is a plain local cache hit; the neighbour is idle.
	before := peerHits.Load()
	p.ServeModule(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/"+testMod+"/@v/"+testVer+".mod", nil))
	if peerHits.Load() != before {
		t.Error("cache hit still asked the neighbour")
	}
}

func TestProxy_NeighbourMissFallsBackUpstream(t *testing.T) {
	t.Parallel()
	var peerHits atomic.Int64
	peer := peerServer(t, nil, nil, &peerHits) // knows nothing, 404s
	p := neighbourEnv(t, peer.URL)

	rec := httptest.NewRecorder()
	p.ServeModule(rec, httptest.NewRequest(http.MethodGet, "/"+testMod+"/@v/"+testVer+".mod", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	if peerHits.Load() == 0 {
		t.Fatal("neighbour was never asked")
	}

	waitFor(t, func() bool { return p.stats.Snapshot().Lifetime.Misses == 1 })
	tot := p.stats.Snapshot().Lifetime
	if tot.NeighbourHits != 0 || tot.NeighbourMisses != 1 {
		t.Errorf("neighbour hits/misses = %d/%d, want 0/1", tot.NeighbourHits, tot.NeighbourMisses)
	}
	if tot.BytesUp == 0 {
		t.Error("upstream fallback fetched nothing")
	}
	if tot.BytesNeighbour != 0 {
		t.Errorf("bytesNeighbour = %d, want 0", tot.BytesNeighbour)
	}
}

func TestProxy_NeighboursQueriedInParallel(t *testing.T) {
	t.Parallel()
	modBytes := []byte("module " + testMod + "\n\ngo 1.21\n")

	var cancelled atomic.Bool
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		cancelled.Store(true)
	}))
	t.Cleanup(slow.Close)

	peer := peerServer(t, modBytes, nil, nil)
	// The slow peer is listed first: only a parallel, cancel-on-win lookup
	// returns before it lets go.
	p := neighbourEnv(t, slow.URL, peer.URL)

	start := time.Now()
	rec := httptest.NewRecorder()
	p.ServeModule(rec, httptest.NewRequest(http.MethodGet, "/"+testMod+"/@v/"+testVer+".mod", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %v: neighbours are not queried in parallel", took)
	}
	waitFor(t, cancelled.Load)
}

func TestProxy_NeighbourTamperedContentRefused(t *testing.T) {
	t.Parallel()
	// A structurally valid zip for the right module@version whose contents
	// do not match the checksum record: only verification can refuse it.
	var wrong bytes.Buffer
	zw := zip.NewWriter(&wrong)
	w, err := zw.Create(testMod + "@" + testVer + "/go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("module example.com/evil\n\ngo 1.21\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	tampered := wrong.Bytes()

	peer := peerServer(t, nil, tampered, nil)
	p := neighbourEnv(t, peer.URL)

	rec := httptest.NewRecorder()
	p.ServeModule(rec, httptest.NewRequest(http.MethodGet, "/"+testMod+"/@v/"+testVer+".zip", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for tampered neighbour content", rec.Code)
	}
	if p.store.Get(testMod, testVer, store.KindZip) != nil {
		t.Fatal("tampered neighbour content was cached")
	}
	if got := p.stats.Snapshot().Lifetime.VerifyFailures; got != 1 {
		t.Errorf("verify failures = %d, want 1", got)
	}
}

func TestNewRejectsBadNeighbour(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	for _, bad := range []string{"not a url", "ftp://example.com", "example.com/without-scheme"} {
		if _, err := New(env.store, env.sumdbDB, env.stats, env.upstreamURL,
			[]string{bad}, true, env.proxy.client, slog.New(slog.DiscardHandler)); err == nil {
			t.Errorf("New accepted neighbour %q", bad)
		}
	}
}

func TestProxy_NeighbourDuplicateIgnored(t *testing.T) {
	t.Parallel()
	modBytes := []byte("module " + testMod + "\n\ngo 1.21\n")
	var peerHits atomic.Int64
	peer := peerServer(t, modBytes, nil, &peerHits)
	p := neighbourEnv(t, peer.URL, peer.URL, peer.URL+"/")
	if got := len(p.neighbours); got != 1 {
		t.Fatalf("neighbours = %d after dedupe, want 1", got)
	}
	rec := httptest.NewRecorder()
	p.ServeModule(rec, httptest.NewRequest(http.MethodGet, "/"+testMod+"/@v/"+testVer+".mod", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := peerHits.Load(); got != 1 {
		t.Errorf("peer saw %d requests, want 1", got)
	}
}
