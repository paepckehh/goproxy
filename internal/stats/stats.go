// Package stats tracks proxy operation: lifetime counters, a recent event
// ring, per-second traffic buckets, and persistence across restarts via an
// atomic JSON snapshot. The hot path only touches atomics and a buffered
// channel, so recording never blocks a request.
package stats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	eventRingSize   = 512
	trafficRingSize = 120 // two minutes of one-second buckets
	eventChanSize   = 1024
	saveInterval    = 30 * time.Second
	tickInterval    = time.Second
)

// Event is one operational event for the dashboard feed.
type Event struct {
	Time    time.Time `json:"time"`
	Module  string    `json:"module,omitempty"`
	Version string    `json:"version,omitempty"`
	Kind    string    `json:"kind,omitempty"`
	Result  string    `json:"result"`
	Status  int       `json:"status,omitempty"`
	Bytes   int64     `json:"bytes,omitempty"`
	DurMS   int64     `json:"durMs,omitempty"`
	Detail  string    `json:"detail,omitempty"`
}

// Traffic is one second of request activity.
type Traffic struct {
	Time   time.Time `json:"time"`
	Hits   int64     `json:"hits"`
	Misses int64     `json:"misses"`
	Bytes  int64     `json:"bytes"`
}

// Totals are the lifetime operational counters.
type Totals struct {
	Requests       int64 `json:"requests"`
	Hits           int64 `json:"hits"`
	Misses         int64 `json:"misses"`
	Passes         int64 `json:"passes"`
	BytesDown      int64 `json:"bytesDown"`
	BytesUp        int64 `json:"bytesUp"`
	BytesSaved     int64 `json:"bytesSaved"`
	Verifications  int64 `json:"verifications"`
	VerifyFailures int64 `json:"verifyFailures"`
}

// Snapshot is the dashboard-facing state.
type Snapshot struct {
	StartedAt     time.Time `json:"startedAt"`
	UptimeSec     int64     `json:"uptimeSec"`
	Lifetime      Totals    `json:"lifetime"`
	Session       Totals    `json:"session"`
	Traffic       []Traffic `json:"traffic"`
	Events        []Event   `json:"events"`
	DroppedEvents int64     `json:"droppedEvents"`
	Started       bool      `json:"started"`
}

// Stats collects proxy metrics. Counters are lock-free; events flow through
// a channel to a background collector started by Run.
type Stats struct {
	started time.Time
	path    string

	requests      atomic.Int64
	hits          atomic.Int64
	misses        atomic.Int64
	passes        atomic.Int64
	bytesDown     atomic.Int64
	bytesUp       atomic.Int64
	bytesSaved    atomic.Int64
	verifications atomic.Int64
	verifyFailed  atomic.Int64
	dropped       atomic.Int64

	evCh    chan Event
	evMu    sync.Mutex
	events  []Event
	tMu     sync.Mutex
	traffic []Traffic

	loaded Totals // totals restored from the snapshot at startup

	tickInterval time.Duration
	saveInterval time.Duration
}

// New creates a collector. If path is non-empty, totals and events persist
// there as a JSON snapshot; call Load before Run to restore them.
func New(path string) *Stats {
	return &Stats{
		started:      time.Now().UTC(),
		path:         path,
		evCh:         make(chan Event, eventChanSize),
		tickInterval: tickInterval,
		saveInterval: saveInterval,
	}
}

// Load restores lifetime totals and recent events from the snapshot.
func (s *Stats) Load() error {
	if s.path == "" {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stats: %w", err)
	}
	var snap struct {
		Totals Totals
		Events []Event
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("stats: snapshot: %w", err)
	}
	s.loaded = snap.Totals
	s.requests.Add(snap.Totals.Requests)
	s.hits.Add(snap.Totals.Hits)
	s.misses.Add(snap.Totals.Misses)
	s.passes.Add(snap.Totals.Passes)
	s.bytesDown.Add(snap.Totals.BytesDown)
	s.bytesUp.Add(snap.Totals.BytesUp)
	s.bytesSaved.Add(snap.Totals.BytesSaved)
	s.verifications.Add(snap.Totals.Verifications)
	s.verifyFailed.Add(snap.Totals.VerifyFailures)
	s.evMu.Lock()
	s.events = append(s.events, snap.Events...)
	s.evMu.Unlock()
	return nil
}

// Run consumes events and maintains traffic buckets until ctx is done,
// then flushes and persists the final snapshot. Run blocks.
func (s *Stats) Run(ctx context.Context) {
	tick := time.NewTicker(s.tickInterval)
	save := time.NewTicker(s.saveInterval)
	defer tick.Stop()
	defer save.Stop()

	// Baseline the tick diffs at the already-loaded lifetime counters,
	// so a restart does not emit one giant synthetic bucket.
	lastHits, lastMisses, lastBytes := s.hits.Load(), s.misses.Load(), s.bytesDown.Load()
	for {
		select {
		case ev := <-s.evCh:
			s.append(ev)
		case <-tick.C:
			h, m, b := s.hits.Load(), s.misses.Load(), s.bytesDown.Load()
			s.tMu.Lock()
			s.traffic = append(s.traffic, Traffic{
				Time:   time.Now().UTC(),
				Hits:   h - lastHits,
				Misses: m - lastMisses,
				Bytes:  b - lastBytes,
			})
			if len(s.traffic) > trafficRingSize {
				s.traffic = s.traffic[len(s.traffic)-trafficRingSize:]
			}
			s.tMu.Unlock()
			lastHits, lastMisses, lastBytes = h, m, b
		case <-save.C:
			s.Save()
		case <-ctx.Done():
			for {
				select {
				case ev := <-s.evCh:
					s.append(ev)
				default:
					s.Save()
					return
				}
			}
		}
	}
}

func (s *Stats) append(ev Event) {
	s.evMu.Lock()
	s.events = append(s.events, ev)
	if len(s.events) > eventRingSize {
		s.events = s.events[len(s.events)-eventRingSize:]
	}
	s.evMu.Unlock()
}

// Hit records a cache hit serving the given byte count.
func (s *Stats) Hit(mod, ver, kind string, size int64, dur time.Duration) {
	s.requests.Add(1)
	s.hits.Add(1)
	s.bytesDown.Add(size)
	s.bytesSaved.Add(size)
	s.send(Event{
		Time: time.Now().UTC(), Module: mod, Version: ver, Kind: kind,
		Result: "hit", Bytes: size, DurMS: dur.Milliseconds(),
	})
}

// Miss records a cache miss that fetched the content from upstream and
// served it downstream.
func (s *Stats) Miss(mod, ver, kind string, fetched, served int64, dur time.Duration, verified bool) {
	s.requests.Add(1)
	s.misses.Add(1)
	s.bytesUp.Add(fetched)
	s.bytesDown.Add(served)
	if verified {
		s.verifications.Add(1)
	}
	s.send(Event{
		Time: time.Now().UTC(), Module: mod, Version: ver, Kind: kind,
		Result: "miss", Bytes: fetched, DurMS: dur.Milliseconds(),
		Detail: verifiedText(verified),
	})
}

// Pass records a non-cacheable passthrough (list, latest, sumdb).
func (s *Stats) Pass(mod, what string, size int64, dur time.Duration) {
	s.requests.Add(1)
	s.passes.Add(1)
	s.bytesDown.Add(size)
	s.bytesUp.Add(size)
	s.send(Event{
		Time: time.Now().UTC(), Module: mod, Kind: what,
		Result: "pass", Bytes: size, DurMS: dur.Milliseconds(),
	})
}

// UpstreamStatus records a non-200 upstream response that was passed through.
func (s *Stats) UpstreamStatus(mod, ver, kind string, status int, dur time.Duration) {
	s.requests.Add(1)
	s.send(Event{
		Time: time.Now().UTC(), Module: mod, Version: ver, Kind: kind,
		Result: "upstream", Status: status, DurMS: dur.Milliseconds(),
	})
}

// VerifyFailure records a checksum verification failure (content refused).
func (s *Stats) VerifyFailure(mod, ver string, detail string) {
	s.requests.Add(1)
	s.verifyFailed.Add(1)
	s.send(Event{
		Time: time.Now().UTC(), Module: mod, Version: ver,
		Result: "verify-failed", Detail: detail,
	})
}

// Error records an internal error serving a request.
func (s *Stats) Error(mod, what, detail string) {
	s.requests.Add(1)
	s.send(Event{
		Time: time.Now().UTC(), Module: mod, Kind: what,
		Result: "error", Detail: detail,
	})
}

// Started records the server startup.
func (s *Stats) Started(version, addr string) {
	s.send(Event{
		Time: time.Now().UTC(), Result: "startup",
		Detail: "goproxy " + version + " listening on " + addr,
	})
}

// Indexed records the startup cache scan result.
func (s *Stats) Indexed(modules int) {
	s.send(Event{
		Time: time.Now().UTC(), Result: "startup",
		Detail: fmt.Sprintf("index rebuilt: %d modules from disk", modules),
	})
}

func verifiedText(v bool) string {
	if v {
		return "verified against checksum db"
	}
	return "unverified: not in checksum db"
}

func (s *Stats) send(ev Event) {
	select {
	case s.evCh <- ev:
	default:
		s.dropped.Add(1)
	}
}

// Snapshot returns the current state for the dashboard.
func (s *Stats) Snapshot() Snapshot {
	t := s.totals()
	s.evMu.Lock()
	events := make([]Event, len(s.events))
	copy(events, s.events)
	s.evMu.Unlock()
	s.tMu.Lock()
	traffic := make([]Traffic, len(s.traffic))
	copy(traffic, s.traffic)
	s.tMu.Unlock()
	// Newest first for the feed.
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}
	if len(events) > 128 {
		events = events[:128]
	}
	return Snapshot{
		StartedAt:     s.started,
		UptimeSec:     int64(time.Since(s.started).Seconds()),
		Lifetime:      t,
		Session:       subtract(t, s.loaded),
		Traffic:       traffic,
		Events:        events,
		DroppedEvents: s.dropped.Load(),
		Started:       true,
	}
}

func (s *Stats) totals() Totals {
	return Totals{
		Requests:       s.requests.Load(),
		Hits:           s.hits.Load(),
		Misses:         s.misses.Load(),
		Passes:         s.passes.Load(),
		BytesDown:      s.bytesDown.Load(),
		BytesUp:        s.bytesUp.Load(),
		BytesSaved:     s.bytesSaved.Load(),
		Verifications:  s.verifications.Load(),
		VerifyFailures: s.verifyFailed.Load(),
	}
}

func subtract(a, b Totals) Totals {
	return Totals{
		Requests:       a.Requests - b.Requests,
		Hits:           a.Hits - b.Hits,
		Misses:         a.Misses - b.Misses,
		Passes:         a.Passes - b.Passes,
		BytesDown:      a.BytesDown - b.BytesDown,
		BytesUp:        a.BytesUp - b.BytesUp,
		BytesSaved:     a.BytesSaved - b.BytesSaved,
		Verifications:  a.Verifications - b.Verifications,
		VerifyFailures: a.VerifyFailures - b.VerifyFailures,
	}
}

// Save persists the snapshot atomically.
func (s *Stats) Save() error {
	if s.path == "" {
		return nil
	}
	s.evMu.Lock()
	events := make([]Event, len(s.events))
	copy(events, s.events)
	s.evMu.Unlock()
	data, err := json.MarshalIndent(struct {
		Totals Totals  `json:"totals"`
		Events []Event `json:"events"`
	}{Totals: s.totals(), Events: events}, "", "  ")
	if err != nil {
		return fmt.Errorf("stats: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("stats: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".stats-*")
	if err != nil {
		return fmt.Errorf("stats: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("stats: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("stats: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return fmt.Errorf("stats: %w", err)
	}
	return nil
}
