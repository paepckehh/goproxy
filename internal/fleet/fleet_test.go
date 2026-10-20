package fleet

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestMonitor_ProbeReportsPeers(t *testing.T) {
	t.Parallel()
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, "ok v9.9.9\n")
	}))
	t.Cleanup(healthy.Close)
	unhealthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	t.Cleanup(unhealthy.Close)
	gone := httptest.NewServer(http.NotFoundHandler())
	goneURL := gone.URL
	gone.Close()

	m := New([]string{healthy.URL + "/", unhealthy.URL, goneURL, "not a url", healthy.URL}, nil, testLogger())
	m.Probe(context.Background())

	got := m.Statuses()
	if len(got) != 3 {
		t.Fatalf("statuses = %d, want 3 (invalid and duplicate entries dropped): %+v", len(got), got)
	}
	if !got[0].Online || got[0].Version != "v9.9.9" {
		t.Errorf("healthy peer = %+v, want online v9.9.9", got[0])
	}
	if got[0].URL != healthy.URL {
		t.Errorf("peer url = %q, want %q", got[0].URL, healthy.URL)
	}
	if got[0].CheckedAt.IsZero() {
		t.Error("healthy peer has no check timestamp")
	}
	if got[1].Online || got[1].Err == "" {
		t.Errorf("unhealthy peer = %+v, want offline with an error", got[1])
	}
	if got[2].Online || got[2].Err == "" {
		t.Errorf("unreachable peer = %+v, want offline with an error", got[2])
	}

	// Statuses must be a copy: a caller cannot mutate the monitor's state.
	got[0].Online = false
	if !m.Statuses()[0].Online {
		t.Error("Statuses exposed the monitor's internal slice")
	}
}

func TestMonitor_RunProbesImmediatelyAndStops(t *testing.T) {
	t.Parallel()
	hits := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits <- struct{}{}
		fmt.Fprint(w, "ok v1\n")
	}))
	t.Cleanup(srv.Close)

	m := New([]string{srv.URL}, nil, testLogger())
	m.interval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx)
	}()

	select {
	case <-hits:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("startup probe never reached the peer")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on context cancellation")
	}
}

func TestMonitor_NoPeers(t *testing.T) {
	t.Parallel()
	m := New(nil, nil, testLogger())
	m.Probe(context.Background())
	if got := m.Statuses(); len(got) != 0 {
		t.Errorf("statuses = %+v, want none", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.Run(ctx) // must return at once rather than blocking on a ticker
}

func TestMonitor_RedactsPeerCredentials(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok v1\n")
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("builder", "s3cret")

	m := New([]string{u.String()}, nil, testLogger())
	m.Probe(context.Background())
	got := m.Statuses()
	if len(got) != 1 || !got[0].Online {
		t.Fatalf("statuses = %+v, want one online peer", got)
	}
	if strings.Contains(got[0].URL, "s3cret") {
		t.Errorf("status url %q leaks the peer password", got[0].URL)
	}
	if !strings.Contains(got[0].URL, "builder") {
		t.Errorf("status url %q lost the peer identity", got[0].URL)
	}
}
