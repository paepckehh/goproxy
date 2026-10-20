package proxy

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// versionQueryUpstream answers the version queries the go command sends
// for branches, short tags and commit hashes.
func versionQueryUpstream(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/" + testMod + "/@v/main.info":
			fmt.Fprint(w, `{"Version":"`+testVer2+`","Time":"2026-01-01T00:00:00Z"}`)
		case "/" + testMod + "/@v/v1.2.info":
			fmt.Fprint(w, `{"Version":"v1.2.0","Time":"2026-01-01T00:00:00Z"}`)
		case "/" + testMod + "/@v/abcdef123456.info":
			fmt.Fprint(w, `{"Version":"v0.0.0-20260101000000-abcdef123456","Time":"2026-01-01T00:00:00Z"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestProxy_VersionQueriesPassThrough(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	var hits atomic.Int64
	upstream := versionQueryUpstream(t, &hits)
	p, err := New(env.store, env.sumdbDB, env.stats, upstream.URL, nil, false,
		upstream.Client(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ query, want string }{
		{"main", testVer2},
		{"v1.2", "v1.2.0"},
		{"abcdef123456", "v0.0.0-20260101000000-abcdef123456"},
	} {
		rec := httptest.NewRecorder()
		p.ServeModule(rec, httptest.NewRequest(http.MethodGet, "/"+testMod+"/@v/"+tc.query+".info", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, body %q", tc.query, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: body = %q, want resolved version %q", tc.query, rec.Body.String(), tc.want)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store (queries move)", tc.query, got)
		}
	}
	if hits.Load() != 3 {
		t.Errorf("upstream hits = %d, want 3", hits.Load())
	}
	// A resolved query is metadata, not a cached artifact.
	if su := env.store.Summary(); su.Files != 0 {
		t.Errorf("store files = %d, want 0: a query answer must not be cached", su.Files)
	}

	// Only .info resolves a query; .mod and .zip need a canonical version.
	rec := httptest.NewRecorder()
	p.ServeModule(rec, httptest.NewRequest(http.MethodGet, "/"+testMod+"/@v/main.mod", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("branch .mod status = %d, want 400", rec.Code)
	}
}

func TestProxy_InvalidVersionQueryRejected(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	for _, path := range []string{
		"/" + testMod + "/@v/..info",
		"/" + testMod + "/@v/a/../b.info",
		"/" + testMod + "/@v/a%20b.info",
	} {
		rec := httptest.NewRecorder()
		env.proxy.ServeModule(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", path, rec.Code)
		}
	}
}

func TestValidQuery(t *testing.T) {
	t.Parallel()
	for _, q := range []string{"main", "v1.2", "abcdef123456", "release-1.0", "v1.2.3-rc.1"} {
		if !validQuery(q) {
			t.Errorf("validQuery(%q) = false, want true", q)
		}
	}
	for _, q := range []string{"", ".", "..", "../evil", "feature/x", "a b", "a\\b", "a?b", "a%b", "a#b"} {
		if validQuery(q) {
			t.Errorf("validQuery(%q) = true, want false", q)
		}
	}
}
