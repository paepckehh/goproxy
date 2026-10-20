package stats

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// run collects until stop, with fast intervals for tests.
func run(t *testing.T, s *Stats) (stop func()) {
	t.Helper()
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

func fastCollector(rec Recorder) *Stats {
	return New(rec, WithTickInterval(5*time.Millisecond), WithFlushInterval(10*time.Millisecond))
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
	s := fastCollector(nil)
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

func TestStats_NeighbourHitAndMiss(t *testing.T) {
	t.Parallel()
	s := fastCollector(nil)
	stop := run(t, s)
	defer stop()

	s.NeighbourHit("example.com/m", "v1.0.0", "zip", 500, 500, 50*time.Millisecond, true, "goproxy2:9090")
	s.NeighbourMiss()

	waitFor(t, func() bool {
		l := s.Snapshot().Lifetime
		return l.NeighbourHits == 1 && l.NeighbourMisses == 1
	})
	want := Totals{Requests: 1, Misses: 1, BytesDown: 500, BytesNeighbour: 500,
		Verifications: 1, NeighbourHits: 1, NeighbourMisses: 1}
	if got := s.Snapshot().Lifetime; got != want {
		t.Errorf("lifetime = %+v, want %+v", got, want)
	}
	if got := s.Snapshot().Session; got != want {
		t.Errorf("session = %+v, want %+v", got, want)
	}

	waitFor(t, func() bool { return len(s.Snapshot().Events) >= 1 })
	ev := s.Snapshot().Events[0]
	if ev.Result != "neighbour" {
		t.Errorf("event result = %q, want neighbour", ev.Result)
	}
	if !strings.Contains(ev.Detail, "goproxy2:9090") || !strings.Contains(ev.Detail, "verified") {
		t.Errorf("event detail = %q, want source and verification", ev.Detail)
	}
	if ev.Bytes != 500 {
		t.Errorf("event bytes = %d, want 500", ev.Bytes)
	}
}

func TestStats_TrafficBuckets(t *testing.T) {
	t.Parallel()
	s := fastCollector(nil)
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

// fakeRecorder captures recorder calls for inspection.
type fakeRecorder struct {
	mu      sync.Mutex
	buckets []Bucket
	totals  []Totals
	events  []Event
	fail    bool
}

func (f *fakeRecorder) snapshot() (buckets []Bucket, totals []Totals, events []Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buckets, f.totals, f.events
}

func (f *fakeRecorder) SaveTotals(t Totals) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("boom")
	}
	f.totals = append(f.totals, t)
	return nil
}

func (f *fakeRecorder) RecordBucket(b Bucket) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("boom")
	}
	f.buckets = append(f.buckets, b)
	return nil
}

func (f *fakeRecorder) AppendEvents(evs []Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("boom")
	}
	f.events = append(f.events, evs...)
	return nil
}

func TestStats_RecorderFlush(t *testing.T) {
	t.Parallel()
	rec := &fakeRecorder{}
	s := fastCollector(rec)
	stop := run(t, s) // Run baselines bucket deltas at the current counters
	// Wait for the baseline and one flush cycle before recording activity.
	waitFor(t, func() bool {
		_, totals, _ := rec.snapshot()
		return len(totals) >= 1
	})
	s.Hit("example.com/m", "v1.0.0", "zip", 100, time.Millisecond)
	s.Miss("example.com/m", "v1.1.0", "zip", 500, 500, time.Millisecond, true)
	waitFor(t, func() bool {
		_, _, events := rec.snapshot()
		return len(events) >= 2
	})
	stop() // triggers the final flush

	buckets, totals, events := rec.snapshot()
	var hits, bytes int64
	for _, b := range buckets {
		hits += b.Hits
		bytes += b.BytesSaved
	}
	if hits != 1 || bytes != 100 {
		t.Errorf("flushed buckets: hits = %d, bytesSaved = %d, want 1, 100", hits, bytes)
	}
	if n := len(totals); n == 0 {
		t.Error("no totals flushed")
	} else {
		last := totals[n-1]
		if last.Hits != 1 || last.BytesSaved != 100 {
			t.Errorf("last flushed totals = %+v", last)
		}
	}
	if len(events) != 2 {
		t.Errorf("flushed events = %d, want 2", len(events))
	}
}

func TestStats_PreRunActivityIsRecorded(t *testing.T) {
	t.Parallel()
	rec := &fakeRecorder{}
	s := fastCollector(rec)
	s.Hit("example.com/m", "v1.0.0", "zip", 100, time.Millisecond)
	stop := run(t, s)
	defer stop()

	waitFor(t, func() bool {
		buckets, _, _ := rec.snapshot()
		var hits int64
		for _, b := range buckets {
			hits += b.Hits
		}
		return hits == 1
	})
	waitFor(t, func() bool {
		var hits int64
		for _, b := range s.Snapshot().Traffic {
			hits += b.Hits
		}
		return hits == 1
	})
}

func TestStats_EventFieldsAreBounded(t *testing.T) {
	t.Parallel()
	s := New(nil)
	long := strings.Repeat("x", maxEventText*4)
	s.Error(long, long, long)
	ev := <-s.evCh
	for name, got := range map[string]string{
		"module": ev.Module,
		"kind":   ev.Kind,
		"detail": ev.Detail,
	} {
		if len(got) > maxEventText+3 {
			t.Errorf("%s length = %d, want <= %d", name, len(got), maxEventText+3)
		}
	}
}

func TestStats_RecorderErrorsCountedNotFatal(t *testing.T) {
	t.Parallel()
	rec := &fakeRecorder{fail: true}
	s := fastCollector(rec)
	s.Hit("m", "v1.0.0", "zip", 10, time.Millisecond)
	stop := run(t, s)
	waitFor(t, func() bool { return s.Snapshot().PersistErrors > 0 })
	stop()

	snap := s.Snapshot()
	if snap.Lifetime.Hits != 1 {
		t.Errorf("hits = %d, want 1 (recorder errors must not affect counting)", snap.Lifetime.Hits)
	}
}

func TestStats_PendingBacklogCapped(t *testing.T) {
	t.Parallel()
	// A recorder that fails forever: the retry backlog must stop growing.
	s := New(&fakeRecorder{fail: true})
	for range pendingMax + 100 {
		s.append(Event{Result: "hit"})
	}
	if len(s.pending) != pendingMax {
		t.Errorf("pending backlog = %d, want capped at %d", len(s.pending), pendingMax)
	}
	if got := s.Snapshot().DroppedEvents; got != 100 {
		t.Errorf("dropped events = %d, want 100", got)
	}
}

func TestStats_SeedContinuesLifetimeAndFeed(t *testing.T) {
	t.Parallel()
	s := New(nil)
	old := Totals{Requests: 42, Hits: 40, BytesSaved: 4000}
	events := []Event{
		{Time: time.Now().Add(-2 * time.Minute).UTC(), Result: "hit", Module: "m"},
		{Time: time.Now().Add(-1 * time.Minute).UTC(), Result: "miss", Module: "m"},
	}
	s.Seed(old, events) // newest first

	stop := run(t, s)
	defer stop()
	waitFor(t, func() bool { return len(s.Snapshot().Events) >= 2 })

	s.Hit("m", "v1.0.0", "zip", 10, time.Millisecond)
	waitFor(t, func() bool { return len(s.Snapshot().Events) >= 3 })

	snap := s.Snapshot()
	if snap.Lifetime.Hits != 41 || snap.Lifetime.Requests != 43 {
		t.Errorf("lifetime = %+v, want hits 41 requests 43", snap.Lifetime)
	}
	if snap.Session != (Totals{Requests: 1, Hits: 1, BytesDown: 10, BytesSaved: 10}) {
		t.Errorf("session = %+v, want only the new hit", snap.Session)
	}
	if snap.Events[0].Result != "hit" {
		t.Errorf("newest event = %q, want hit", snap.Events[0].Result)
	}
}

func TestStats_EventDropNeverBlocks(t *testing.T) {
	t.Parallel()
	s := New(nil) // Run never started: channel fills up.

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

func TestStats_SubscribeReceivesLiveEvents(t *testing.T) {
	t.Parallel()
	s := fastCollector(nil)
	stop := run(t, s)
	defer stop()

	ch, unsubscribe := s.Subscribe()
	events := make(chan Event, 4)
	go func() {
		for ev := range ch {
			events <- ev
		}
	}()

	s.Hit("example.com/m", "v1.0.0", "zip", 128, time.Millisecond)
	select {
	case ev := <-events:
		if ev.Result != "hit" || ev.Module != "example.com/m" || ev.Bytes != 128 {
			t.Errorf("event = %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber did not receive the event")
	}

	unsubscribe()
	waitFor(t, func() bool {
		select {
		case _, open := <-ch:
			return !open
		default:
			return false
		}
	})
}

func TestStats_SubscriberOverflowDoesNotBlock(t *testing.T) {
	t.Parallel()
	s := fastCollector(nil)
	stop := run(t, s)
	defer stop()

	ch, unsubscribe := s.Subscribe() // never drained
	defer unsubscribe()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range subscriberBuffer * 4 {
			s.Hit("example.com/m", "v1.0.0", "zip", 1, 0)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("collector blocked on a slow subscriber")
	}
	waitFor(t, func() bool { return len(ch) == subscriberBuffer })
	if len(ch) > subscriberBuffer {
		t.Errorf("buffered events = %d, want at most %d", len(ch), subscriberBuffer)
	}
}

func TestStats_PrewarmFormatsBandwidth(t *testing.T) {
	t.Parallel()
	s := New(nil)
	s.Prewarm(100, 12, 1, 3<<30, 1<<30)
	ev := <-s.evCh
	if !strings.Contains(ev.Detail, "4.0 GiB pulled") {
		t.Errorf("prewarm detail = %q, want a human-readable bandwidth", ev.Detail)
	}
}

func TestStats_WithStartedAt(t *testing.T) {
	t.Parallel()
	start := time.Now().Add(-3 * time.Hour).UTC()
	s := New(nil, WithStartedAt(start))
	if !s.started.Equal(start) {
		t.Errorf("started = %v, want %v", s.started, start)
	}
}
