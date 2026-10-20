package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestConnTrackerStates(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	tr := &connTracker{}
	steps := []struct {
		state http.ConnState
		busy  int64
	}{
		{http.StateNew, 0},    // accepted, not yet serving
		{http.StateActive, 1}, // request in flight
		{http.StateIdle, 0},   // keep-alive between requests
		{http.StateActive, 1}, // keep-alive reused
		{http.StateIdle, 0},   // between requests again
		{http.StateClosed, 0}, // gone
	}
	for _, s := range steps {
		tr.state(c1, s.state)
		if got := tr.busy.Load(); got != s.busy {
			t.Errorf("state %v: busy = %d, want %d", s.state, got, s.busy)
		}
	}
}

func TestConnTrackerOverlapping(t *testing.T) {
	a1, a2 := net.Pipe()
	defer a1.Close()
	defer a2.Close()
	b1, b2 := net.Pipe()
	defer b1.Close()
	defer b2.Close()

	tr := &connTracker{}
	tr.state(a1, http.StateActive)
	tr.state(b1, http.StateActive)
	if got := tr.busy.Load(); got != 2 {
		t.Fatalf("busy = %d, want 2", got)
	}
	tr.state(a1, http.StateClosed)
	if got := tr.busy.Load(); got != 1 {
		t.Fatalf("busy after first close = %d, want 1", got)
	}
	tr.state(b1, http.StateIdle)
	if got := tr.busy.Load(); got != 0 {
		t.Fatalf("busy after second idle = %d, want 0", got)
	}
}

func TestConnTrackerHTTP(t *testing.T) {
	tr := &connTracker{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tr.busy.Load() != 1 {
			t.Errorf("busy during request = %d, want 1", tr.busy.Load())
		}
	}))
	srv.Config.ConnState = tr.state
	srv.Start()
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// The keep-alive connection stays open but idle: not busy. The server
	// reports the transition asynchronously, so give it a moment.
	deadline := time.Now().Add(2 * time.Second)
	for tr.busy.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := tr.busy.Load(); got != 0 {
		t.Errorf("busy after request = %d, want 0", got)
	}
}

func TestWatchIdleExitsWhenIdle(t *testing.T) {
	tr := &connTracker{}
	done := watchIdle(context.Background(), tr, 50*time.Millisecond)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watchIdle did not fire for an idle server")
	}
}

func TestWatchIdleStaysWhileBusy(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	tr := &connTracker{}
	tr.state(c1, http.StateActive)

	done := watchIdle(context.Background(), tr, 50*time.Millisecond)
	select {
	case <-done:
		t.Fatal("watchIdle fired while a request was in flight")
	case <-time.After(500 * time.Millisecond):
	}
}

func TestWatchIdleStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tr := &connTracker{}
	done := watchIdle(ctx, tr, 50*time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watchIdle did not stop on ctx cancel")
	}
}

func TestEnvBool(t *testing.T) {
	for _, tt := range []struct {
		val  string
		want bool
	}{
		{"1", true},
		{"true", true},
		{"Yes", true},
		{" ON ", true},
		{"", false},
		{"0", false},
		{"false", false},
		{"no", false},
		{"junk", false},
	} {
		t.Setenv("GOPROXY_TEST_ENV_BOOL", tt.val)
		if got := envBool("GOPROXY_TEST_ENV_BOOL"); got != tt.want {
			t.Errorf("envBool(%q) = %v, want %v", tt.val, got, tt.want)
		}
	}
	if envBool("GOPROXY_TEST_ENV_UNSET") {
		t.Error("envBool(unset) = true, want false")
	}
	os.Unsetenv("GOPROXY_TEST_ENV_BOOL")
}
