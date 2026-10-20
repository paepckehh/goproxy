package main

// Full-stack end-to-end test: real upstream, real checksum database, the
// real dispatcher, dashboard API and SSE feed. Opt-in via GOPROXY_E2E=1
// (make test-e2e); skips without network access.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"paepcke.de/goproxy/internal/history"
	"paepcke.de/goproxy/internal/proxy"
	"paepcke.de/goproxy/internal/stats"
	"paepcke.de/goproxy/internal/store"
	"paepcke.de/goproxy/internal/sumdb"
	"paepcke.de/goproxy/internal/ui"
)

const (
	e2eModule  = "rsc.io/quote"
	e2eVersion = "v1.5.2"
	e2eZipHash = "w5fcysjrx7yqtD/aO+QwRjYZOKnaM9Uh2b40tElTs3Y="
)

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

// TestE2E_FullStackFetchesRealModule drives the same handler tree the
// binary serves: a module request is fetched from proxy.golang.org and
// verified against sum.golang.org, then shows up in the dashboard API and
// is pushed to an already connected SSE client.
func TestE2E_FullStackFetchesRealModule(t *testing.T) {
	requireE2E(t, "proxy.golang.org")

	root := t.TempDir()
	st, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := history.Open(filepath.Join(root, "stats.db"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sumdb.New("https://sum.golang.org", sumdb.DefaultKey,
		filepath.Join(root, "sumdb"), sumdbClient(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	m := stats.New(hist)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(runDone)
	}()
	t.Cleanup(func() {
		cancel()
		<-runDone
		if err := hist.Close(); err != nil {
			t.Errorf("closing stats db: %v", err)
		}
	})
	p, err := proxy.New(st, db, m, "https://proxy.golang.org", nil, true, moduleClient(),
		slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	const version = "v0.0.0-e2e"
	uiH := ui.New(m, st, hist, version, nil, nil)
	srv := httptest.NewServer(dispatch(p, uiH, version))
	t.Cleanup(srv.Close)

	client := &http.Client{Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}, Timeout: 2 * time.Minute}

	// Connect the live feed first: the fetch below must be pushed to it.
	sseCtx, sseCancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer sseCancel()
	sseReq, err := http.NewRequestWithContext(sseCtx, http.MethodGet, srv.URL+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	sseResp, err := client.Do(sseReq)
	if err != nil {
		t.Fatal(err)
	}
	defer sseResp.Body.Close()
	stream := bufio.NewReader(sseResp.Body)
	if line, err := stream.ReadString('\n'); err != nil || !strings.HasPrefix(line, ": connected") {
		t.Fatalf("event stream did not open: line %q, err %v", line, err)
	}

	// Fetch the go.mod through the real dispatcher.
	resp, err := client.Get(srv.URL + "/" + e2eModule + "/@v/" + e2eVersion + ".mod")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mod: status = %d, body = %q", resp.StatusCode, body)
	}
	if !strings.HasPrefix(string(body), "module ") || !strings.Contains(string(body), e2eModule) {
		t.Errorf("mod body = %q", body)
	}
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("cache-control = %q", got)
	}
	mod := st.Get(e2eModule, e2eVersion, store.KindMod)
	if mod == nil || !mod.Verified {
		t.Errorf("go.mod not cached and verified: %+v", mod)
	}

	// The connected dashboard receives the miss as a pushed event. The
	// stream also seeds and periodically pushes state messages, so only
	// "event" messages are decoded.
	var event stats.Event
	var kind string
	for {
		line, err := stream.ReadString('\n')
		if err != nil {
			t.Fatalf("waiting for the pushed event: %v", err)
		}
		line = strings.TrimSpace(line)
		if name, ok := strings.CutPrefix(line, "event: "); ok {
			kind = name
			continue
		}
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || kind != "event" {
			continue
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			t.Fatalf("event payload %q: %v", payload, err)
		}
		if event.Module == e2eModule {
			break
		}
	}
	if event.Result != "miss" || event.Version != e2eVersion || event.Kind != "mod" {
		t.Errorf("pushed event = %+v", event)
	}

	// Dashboard JSON reflects the fetch and the cached module.
	resp, err = client.Get(srv.URL + "/api/dashboard")
	if err != nil {
		t.Fatal(err)
	}
	var dash struct {
		Version string `json:"version"`
		Stats   struct {
			Lifetime struct {
				Requests int64 `json:"requests"`
				Misses   int64 `json:"misses"`
			} `json:"lifetime"`
			Events []stats.Event `json:"events"`
		} `json:"stats"`
		Modules []store.ModStat `json:"modules"`
	}
	err = json.NewDecoder(resp.Body).Decode(&dash)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if dash.Version != version {
		t.Errorf("version = %q, want %q", dash.Version, version)
	}
	if dash.Stats.Lifetime.Requests < 1 || dash.Stats.Lifetime.Misses < 1 {
		t.Errorf("lifetime = %+v, want at least one miss", dash.Stats.Lifetime)
	}
	if len(dash.Stats.Events) == 0 || dash.Stats.Events[0].Module != e2eModule {
		t.Errorf("dashboard events = %+v", dash.Stats.Events)
	}
	var found bool
	for _, mod := range dash.Modules {
		if mod.Path == e2eModule {
			found = true
			if !mod.Verified {
				t.Error("dashboard reports the module as unverified")
			}
		}
	}
	if !found {
		t.Errorf("module missing from the dashboard: %+v", dash.Modules)
	}

	// And the zip really matches the published checksum.
	resp, err = client.Get(srv.URL + "/" + e2eModule + "/@v/" + e2eVersion + ".zip")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("zip: status = %d", resp.StatusCode)
	}
	zip := st.Get(e2eModule, e2eVersion, store.KindZip)
	if zip == nil || !zip.Verified || zip.Hash != e2eZipHash {
		t.Errorf("zip = %+v, want verified with %s", zip, e2eZipHash)
	}

	// healthz stays a plain liveness endpoint.
	resp, err = client.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	health, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(health), version) {
		t.Errorf("healthz: status = %d, body = %q", resp.StatusCode, health)
	}
}
