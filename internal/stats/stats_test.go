package stats

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// run collects until stop, with fast intervals for tests.
func run(t *testing.T, s *Stats) (stop func()) {
	t.Helper()
	s.tickInterval = 5 * time.Millisecond
	s.saveInterval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	return func() {
		cancel()
		<-done
	}
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

func TestStats_CountersAndEvents(t *testing.T) {
	t.Parallel()
	s := New("")
	stop := run(t, s)
	defer stop()

	s.Hit("example.com/m", "v1.0.0", "zip", 100, 5*time.Millisecond)
	s.Miss("example.com/m", "v1.1.0", "zip", 500, 500, 50*time.Millisecond, true)
	s.Pass("example.com/m", "list", 10, 1*time.Millisecond)
	s.VerifyFailure("example.com/bad", "v2.0.0", "hash mismatch")

	waitFor(t, func() bool { return len(s.Snapshot().Events) >= 4 })

	snap := s.Snapshot()
	want := Totals{Requests: 4, Hits: 1, Misses: 1, Passes: 1,
		BytesDown: 610, BytesUp: 510, BytesSaved: 100,
		Verifications: 1, VerifyFailures: 1}
	if snap.Lifetime != want {
		t.Errorf("lifetime = %+v, want %+v", snap.Lifetime, want)
	}
	if snap.Session != want {
		t.Errorf("session = %+v, want %+v", snap.Session, want)
	}
	if len(snap.Events) != 4 {
		t.Fatalf("events = %d, want 4", len(snap.Events))
	}
	if snap.Events[0].Result != "verify-failed" {
		t.Errorf("newest event = %q, want verify-failed", snap.Events[0].Result)
	}
}

func TestStats_TrafficBuckets(t *testing.T) {
	t.Parallel()
	s := New("")
	stop := run(t, s)
	defer stop()

	// Let Run take its baseline first, then record the hit.
	waitFor(t, func() bool { return len(s.Snapshot().Traffic) >= 1 })
	s.Hit("m", "v1.0.0", "zip", 10, time.Millisecond)
	waitFor(t, func() bool {
		tr := s.Snapshot().Traffic
		var hits int64
		for _, b := range tr {
			hits += b.Hits
		}
		return hits >= 1
	})

	tr := s.Snapshot().Traffic
	var hits int64
	for _, b := range tr {
		hits += b.Hits
	}
	if hits != 1 {
		t.Errorf("total bucket hits = %d, want 1", hits)
	}
	if len(tr) > trafficRingSize {
		t.Errorf("traffic ring grew to %d, max %d", len(tr), trafficRingSize)
	}
}

func TestStats_PersistenceRoundtrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "stats.json")

	s := New(path)
	stop := run(t, s)
	s.Hit("example.com/m", "v1.0.0", "zip", 100, time.Millisecond)
	s.Miss("example.com/m", "v1.1.0", "zip", 500, 500, time.Millisecond, true)
	waitFor(t, func() bool { return len(s.Snapshot().Events) >= 2 })
	stop()
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	fresh := New(path)
	if err := fresh.Load(); err != nil {
		t.Fatal(err)
	}
	snap := fresh.Snapshot()
	if snap.Lifetime.Hits != 1 || snap.Lifetime.BytesSaved != 100 {
		t.Errorf("restored lifetime = %+v", snap.Lifetime)
	}
	if len(snap.Events) != 2 {
		t.Errorf("restored events = %d, want 2", len(snap.Events))
	}

	// Session counters start at zero after restart.
	if snap.Session != (Totals{}) {
		t.Errorf("session after restart = %+v, want zero", snap.Session)
	}
}

func TestStats_EventDropNeverBlocks(t *testing.T) {
	t.Parallel()
	s := New("") // Run never started: channel fills up.

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range eventChanSize * 2 {
			s.Hit("m", "v1.0.0", "zip", 1, 0)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Record blocked on full event channel")
	}

	if s.dropped.Load() == 0 {
		t.Error("dropped events not counted")
	}
	if s.hits.Load() != eventChanSize*2 {
		t.Errorf("hits = %d, want %d", s.hits.Load(), eventChanSize*2)
	}
}

func TestStats_LoadMissingFile(t *testing.T) {
	t.Parallel()
	s := New(filepath.Join(t.TempDir(), "absent.json"))
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().Lifetime != (Totals{}) {
		t.Error("loaded totals from missing file")
	}
}
