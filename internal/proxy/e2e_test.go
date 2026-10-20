package proxy

// End-to-end tests against the real module proxy and checksum database.
// They are opt-in: set GOPROXY_E2E=1 (make test-e2e). Without that, or
// without outbound network access, they skip so the regular suite stays
// hermetic.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/mod/semver"

	"paepcke.de/goproxy/internal/stats"
	"paepcke.de/goproxy/internal/store"
	"paepcke.de/goproxy/internal/sumdb"
)

const (
	e2eUpstreamURL = "https://proxy.golang.org"
	e2eSumDBURL    = "https://sum.golang.org"
	e2eModule      = "rsc.io/quote"
	e2eVersion     = "v1.5.2"
	// Published in sum.golang.org; a mismatch means verification broke.
	e2eZipHash = "w5fcysjrx7yqtD/aO+QwRjYZOKnaM9Uh2b40tElTs3Y="
	e2eModHash = "LzX7hefJvL54yjefDEDHNONDjII0t9xZLPXsUe+TKr0="
)

// requireE2E skips unless the operator opted in and the network is usable.
func requireE2E(t *testing.T, host string) {
	t.Helper()
	if os.Getenv("GOPROXY_E2E") != "1" {
		t.Skip("end-to-end test: set GOPROXY_E2E=1 to fetch from the real upstream")
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, "443"), 10*time.Second)
	if err != nil {
		t.Skipf("end-to-end test: %s unreachable: %v", host, err)
	}
	conn.Close()
}

// newE2EProxy builds a real proxy: real upstream, real checksum database,
// no mocks or fixtures.
func newE2EProxy(t *testing.T) (*Proxy, *store.Store, *stats.Stats) {
	t.Helper()
	root := t.TempDir()
	st, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ForceAttemptHTTP2:     true,
	}}
	db, err := sumdb.New(e2eSumDBURL, sumdb.DefaultKey, filepath.Join(root, "sumdb"), client,
		slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	m := stats.New(nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	p, err := New(st, db, m, e2eUpstreamURL, true, client, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return p, st, m
}

func e2eGet(t *testing.T, p *Proxy, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	p.ServeModule(rec, req)
	return rec
}

// TestE2E_FetchRealModuleAndVerify downloads info, go.mod and the zip of a
// real module from proxy.golang.org and checks that the content matches the
// hashes published by sum.golang.org, that the signed record is cached and
// that the second request is served from cache.
func TestE2E_FetchRealModuleAndVerify(t *testing.T) {
	requireE2E(t, "proxy.golang.org")
	p, st, m := newE2EProxy(t)

	base := "/" + e2eModule + "/@v/" + e2eVersion
	for kind, wantType := range map[store.Kind]string{
		store.KindInfo: "application/json",
		store.KindMod:  "text/plain",
		store.KindZip:  "application/zip",
	} {
		rec := e2eGet(t, p, base+"."+string(kind))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, body = %q", kind, rec.Code, rec.Body.String())
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s: empty body", kind)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, wantType) {
			t.Errorf("%s: content type = %q, want %q", kind, ct, wantType)
		}
	}

	zip := st.Get(e2eModule, e2eVersion, store.KindZip)
	if zip == nil {
		t.Fatal("zip not indexed after a successful fetch")
	}
	if !zip.Verified {
		t.Error("zip not marked verified")
	}
	if zip.Hash != e2eZipHash {
		t.Errorf("zip hash = %q, want %q", zip.Hash, e2eZipHash)
	}
	mod := st.Get(e2eModule, e2eVersion, store.KindMod)
	if mod == nil || !mod.Verified || mod.Hash != e2eModHash {
		t.Errorf("go.mod = %+v, want verified with hash %q", mod, e2eModHash)
	}

	// The signed record is kept next to the module as provenance.
	record, err := os.ReadFile(filepath.Join(st.Root(), "download", e2eModule, "@v", e2eVersion+".sum"))
	if err != nil {
		t.Fatalf("reading the cached checksum record: %v", err)
	}
	if !strings.Contains(string(record), "h1:"+e2eZipHash) {
		t.Errorf("cached record missing the zip hash:\n%s", record)
	}

	// A second request must not touch the network: it is a cache hit.
	rec := e2eGet(t, p, base+".zip")
	if rec.Code != http.StatusOK || rec.Body.Len() != int(zip.Size) {
		t.Fatalf("cached zip: status = %d, bytes = %d, want 200 and %d", rec.Code, rec.Body.Len(), zip.Size)
	}
	waitFor(t, func() bool { return m.Snapshot().Lifetime.Hits == 1 })
}

// TestE2E_ListAndLatestPassThrough checks the mutable endpoints against the
// real upstream: the version list contains the version we can fetch and
// @latest answers with valid JSON for a real module.
func TestE2E_ListAndLatestPassThrough(t *testing.T) {
	requireE2E(t, "proxy.golang.org")
	p, _, _ := newE2EProxy(t)

	rec := e2eGet(t, p, "/"+e2eModule+"/@v/list")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status = %d, body = %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), e2eVersion) {
		t.Errorf("list does not contain %s:\n%s", e2eVersion, rec.Body.String())
	}

	rec = e2eGet(t, p, "/"+e2eModule+"/@latest")
	if rec.Code != http.StatusOK {
		t.Fatalf("latest: status = %d, body = %q", rec.Code, rec.Body.String())
	}
	var latest struct {
		Version string
		Time    time.Time
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &latest); err != nil {
		t.Fatalf("latest: %v (body %q)", err, rec.Body.String())
	}
	if !semver.IsValid(latest.Version) {
		t.Errorf("latest version = %q", latest.Version)
	}
	if latest.Time.IsZero() {
		t.Error("latest time is zero")
	}
}

// TestE2E_UnknownModulePasses404Through makes sure a real upstream 404 is
// forwarded as a 404 instead of being mistaken for an internal failure.
func TestE2E_UnknownModulePasses404Through(t *testing.T) {
	requireE2E(t, "proxy.golang.org")
	p, st, _ := newE2EProxy(t)

	const missing = "paepcke.de/goproxy-e2e-does-not-exist"
	rec := e2eGet(t, p, "/"+missing+"/@v/list")
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusGone {
		t.Fatalf("status = %d, want 404 or 410 (body %q)", rec.Code, rec.Body.String())
	}
	if st.Get(missing, "", store.KindInfo) != nil {
		t.Error("unknown module got cached")
	}
}

// TestE2E_SumDBForwarded proves a go client can run its whole verification
// through this proxy: lookups are forwarded to sum.golang.org and answer
// with the signed record.
func TestE2E_SumDBForwarded(t *testing.T) {
	requireE2E(t, "sum.golang.org")
	p, _, _ := newE2EProxy(t)

	probe := httptest.NewRequest(http.MethodGet, "/sumdb/sum.golang.org/supported", nil)
	rec := httptest.NewRecorder()
	p.ServeSumDB(rec, probe)
	if rec.Code != http.StatusOK {
		t.Errorf("/supported: status = %d, want 200", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet,
		"/sumdb/sum.golang.org/lookup/"+e2eModule+"@"+e2eVersion, nil)
	rec = httptest.NewRecorder()
	p.ServeSumDB(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("lookup: status = %d, body = %q", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "h1:"+e2eZipHash) {
		t.Errorf("lookup record missing the zip hash:\n%s", body)
	}

	foreign := httptest.NewRequest(http.MethodGet, "/sumdb/other.example/lookup/x", nil)
	rec = httptest.NewRecorder()
	p.ServeSumDB(rec, foreign)
	if rec.Code != http.StatusNotFound {
		t.Errorf("foreign database: status = %d, want 404", rec.Code)
	}
}
