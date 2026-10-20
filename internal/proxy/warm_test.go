package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"paepcke.de/goproxy/internal/store"
)

func TestWarmer_SyncCachesLatestOfPopularModules(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	modBytes := []byte("module " + testMod + "\n\ngo 1.21\n")
	zipBytes := buildModuleZip(t, testMod, testVer)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + testMod + "/@latest":
			fmt.Fprint(w, `{"Version":"`+testVer+`","Time":"2025-01-01T00:00:00Z"}`)
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

	p, err := New(env.store, env.sumdbDB, env.stats, upstream.URL, nil, false,
		upstream.Client(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	// Make the module the hottest one without warming its files: the
	// ranking comes from served requests, not from what is cached.
	if _, err := env.store.PutBytes(testMod, testVer, store.KindMod, modBytes, "", false); err != nil {
		t.Fatal(err)
	}
	env.store.Get(testMod, testVer, store.KindMod).RecordHit(1)

	w := NewWarmer(p, env.store.Popular, slog.New(slog.DiscardHandler))
	sum := w.Sync(context.Background())
	if sum.Modules != 1 || sum.Fetched != 1 || sum.Failed != 0 {
		t.Fatalf("summary = %+v, want 1 module fetched, none failed", sum)
	}
	if sum.Bytes <= 0 {
		t.Errorf("bytes = %d, want the downloaded file sizes", sum.Bytes)
	}
	for _, kind := range []store.Kind{store.KindInfo, store.KindMod, store.KindZip} {
		if f := env.store.Get(testMod, testVer, kind); f == nil {
			t.Errorf("%s not cached after prewarm", kind)
		}
	}

	// The report carries the per-module success details the dashboard shows.
	report := w.Report()
	if report.StartedAt.IsZero() || report.DurationMS < 0 {
		t.Errorf("report timing = %+v, want a recorded pass", report)
	}
	if len(report.Items) != 1 {
		t.Fatalf("report items = %+v, want one module", report.Items)
	}
	item := report.Items[0]
	if item.Module != testMod || item.Version != testVer || item.Bytes <= 0 || item.Err != "" {
		t.Errorf("report item = %+v, want a successful prewarm of %s@%s", item, testMod, testVer)
	}

	// A second pass finds everything cached and pulls nothing.
	if sum := w.Sync(context.Background()); sum.Fetched != 0 || sum.Bytes != 0 || sum.Failed != 0 {
		t.Errorf("second summary = %+v, want a no-op pass", sum)
	}
	if got := w.Report().Items; len(got) != 1 || got[0].Bytes != 0 {
		t.Errorf("second report items = %+v, want the module reported as already cached", got)
	}
}

func TestWarmer_SyncWithoutPopularModules(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	w := NewWarmer(env.proxy, env.store.Popular, slog.New(slog.DiscardHandler))
	sum := w.Sync(context.Background())
	if sum.Modules != 0 || sum.Fetched != 0 || sum.Bytes != 0 || sum.Failed != 0 || len(sum.Items) != 0 {
		t.Errorf("summary = %+v, want an empty pass", sum)
	}
	if got := w.Report(); got.StartedAt.IsZero() || len(got.Items) != 0 {
		t.Errorf("report = %+v, want a recorded empty pass", got)
	}
}

func TestWarmer_NextStaysWithinJitter(t *testing.T) {
	t.Parallel()
	w := NewWarmer(nil, nil, slog.New(slog.DiscardHandler))
	lo, hi := PrewarmInterval-PrewarmJitter, PrewarmInterval+PrewarmJitter
	for range 200 {
		if d := w.next(); d < lo || d > hi {
			t.Fatalf("next() = %s, want within [%s, %s]", d, lo, hi)
		}
	}
	w.jitter = 0
	if got := w.next(); got != PrewarmInterval {
		t.Errorf("next() without jitter = %s, want %s", got, PrewarmInterval)
	}
}

func TestWarmer_RunStopsOnCancellation(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	w := NewWarmer(env.proxy, env.store.Popular, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on context cancellation")
	}
}
