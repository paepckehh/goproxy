// Package stats tracks proxy operation: lifetime counters, a recent event
// ring, per-second traffic buckets, and long-term persistence through a
// Recorder (the cgo-free SQLite backend in internal/history). The hot path
// only touches atomics and a buffered channel, so recording never blocks a
// request; the collector goroutine flushes to the recorder in batches.
package stats

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	eventRingSize    = 512
	trafficRingSize  = 120 // two minutes of one-second buckets
	eventChanSize    = 1024
	pendingMax       = 8192 // event retry backlog cap; see append
	maxEventText     = 512  // covers the longest valid module path, bounds hostile input
	defaultTick      = time.Second
	defaultFlush     = 30 * time.Second
	subscriberBuffer = 64
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
	Requests        int64 `json:"requests"`
	Hits            int64 `json:"hits"`
	Misses          int64 `json:"misses"`
	Passes          int64 `json:"passes"`
	BytesDown       int64 `json:"bytesDown"`
	BytesUp         int64 `json:"bytesUp"`
	BytesSaved      int64 `json:"bytesSaved"`
	BytesNeighbour  int64 `json:"bytesNeighbour"`
	Verifications   int64 `json:"verifications"`
	VerifyFailures  int64 `json:"verifyFailures"`
	NeighbourHits   int64 `json:"neighbourHits"`
	NeighbourMisses int64 `json:"neighbourMisses"`
}

// StoreStat is a point-in-time snapshot of the cache store size, sampled
// alongside each history bucket.
type StoreStat struct {
	Bytes    int64 `json:"bytes"`
	Modules  int64 `json:"modules"`
	Versions int64 `json:"versions"`
	Files    int64 `json:"files"`
}

// Bucket is the delta of the lifetime counters over one wall-clock minute
// plus the store size at the end of the interval. Buckets are the long-term
// history: one row per minute, aggregated by the dashboard into hourly and
// daily series.
type Bucket struct {
	Bucket time.Time `json:"bucket"`
	Totals
	Store StoreStat `json:"store"`
}

// Recorder persists stats data outside the request path. Implementations
// must be safe for concurrent use. Errors are counted and surfaced on the
// dashboard (persistErrors); a failing recorder never degrades proxying.
type Recorder interface {
	// SaveTotals upserts the lifetime counters.
	SaveTotals(Totals) error
	// RecordBucket upserts one minute of activity: deltas merge, store
	// snapshots overwrite.
	RecordBucket(Bucket) error
	// AppendEvents appends a batch of events atomically.
	AppendEvents([]Event) error
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
	PersistErrors int64     `json:"persistErrors"`
	Memory        Memory    `json:"memory"`
	Started       bool      `json:"started"`
}

// Stats collects proxy metrics. Counters are lock-free; events flow through
// a channel to a background collector started by Run, which also flushes
// the recorder.
type Stats struct {
	started time.Time
	rec     Recorder

	requests        atomic.Int64
	hits            atomic.Int64
	misses          atomic.Int64
	passes          atomic.Int64
	bytesDown       atomic.Int64
	bytesUp         atomic.Int64
	bytesSaved      atomic.Int64
	bytesNeighbour  atomic.Int64
	neighbourHits   atomic.Int64
	neighbourMisses atomic.Int64
	verifications   atomic.Int64
	verifyFailed    atomic.Int64
	dropped         atomic.Int64
	dbErrs          atomic.Int64

	evCh      chan Event
	evMu      sync.Mutex
	events    []Event
	pending   []Event // events awaiting the next recorder flush; Run goroutine only
	subMu     sync.Mutex
	subs      map[chan Event]struct{}
	tMu       sync.Mutex
	traffic   []Traffic
	flushMu   sync.Mutex
	lastFlush Totals // baseline for bucket deltas

	loaded       Totals // totals restored from the recorder at startup
	storeSampler func() StoreStat

	memMu    sync.Mutex
	mem      Memory
	memAt    time.Time
	memLimit int64

	tickInterval  time.Duration
	flushInterval time.Duration
}

// Option configures a Stats collector.
type Option func(*Stats)

// WithStoreSampler samples the cache store size with each history bucket.
func WithStoreSampler(f func() StoreStat) Option {
	return func(s *Stats) { s.storeSampler = f }
}

// WithTickInterval overrides the per-second traffic bucket interval.
// Non-positive intervals keep the default (time.NewTicker panics on them).
func WithTickInterval(d time.Duration) Option {
	return func(s *Stats) {
		if d > 0 {
			s.tickInterval = d
		}
	}
}

// WithFlushInterval overrides the recorder flush interval. Non-positive
// intervals keep the default (time.NewTicker panics on them).
func WithFlushInterval(d time.Duration) Option {
	return func(s *Stats) {
		if d > 0 {
			s.flushInterval = d
		}
	}
}

// WithStartedAt overrides the process start time reported as uptime.
// Embeddings and screenshot fixtures use it to show a stable, realistic
// uptime instead of the seconds since the collector was created.
func WithStartedAt(t time.Time) Option {
	return func(s *Stats) {
		if !t.IsZero() {
			s.started = t.UTC()
		}
	}
}

// New creates a collector. rec persists lifetime totals, one history
// bucket per minute and the event log; nil disables persistence (counters
// and the feed stay in memory only).
func New(rec Recorder, opts ...Option) *Stats {
	s := &Stats{
		started:       time.Now().UTC(),
		rec:           rec,
		evCh:          make(chan Event, eventChanSize),
		tickInterval:  defaultTick,
		flushInterval: defaultFlush,
		memLimit:      readMemoryLimit(),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Seed restores lifetime totals and the event feed from the recorder,
// continuing lifetime counters and history across restarts. newestFirst
// must be newest-first, as returned by history.DB.RecentEvents.
func (s *Stats) Seed(t Totals, newestFirst []Event) {
	s.loaded = t
	s.requests.Add(t.Requests)
	s.hits.Add(t.Hits)
	s.misses.Add(t.Misses)
	s.passes.Add(t.Passes)
	s.bytesDown.Add(t.BytesDown)
	s.bytesUp.Add(t.BytesUp)
	s.bytesSaved.Add(t.BytesSaved)
	s.bytesNeighbour.Add(t.BytesNeighbour)
	s.neighbourHits.Add(t.NeighbourHits)
	s.neighbourMisses.Add(t.NeighbourMisses)
	s.verifications.Add(t.Verifications)
	s.verifyFailed.Add(t.VerifyFailures)
	s.evMu.Lock()
	for i := len(newestFirst) - 1; i >= 0; i-- {
		s.events = append(s.events, trimEvent(newestFirst[i]))
	}
	if len(s.events) > eventRingSize {
		s.events = s.events[len(s.events)-eventRingSize:]
	}
	s.evMu.Unlock()
}

// Run consumes events, maintains traffic buckets and flushes the recorder
// until ctx is done, then flushes one final time. Run blocks.
func (s *Stats) Run(ctx context.Context) {
	tick := time.NewTicker(s.tickInterval)
	flush := time.NewTicker(s.flushInterval)
	defer tick.Stop()
	defer flush.Stop()

	// Baseline the tick diffs and bucket deltas at the restored lifetime
	// counters, so a restart does not emit one giant synthetic bucket.
	// Activity recorded between Seed and Run still belongs to this
	// session's first bucket and traffic tick.
	lastHits, lastMisses, lastBytes := s.loaded.Hits, s.loaded.Misses, s.loaded.BytesDown
	s.flushMu.Lock()
	s.lastFlush = s.loaded
	s.flushMu.Unlock()
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
		case <-flush.C:
			s.flush()
		case <-ctx.Done():
			for {
				select {
				case ev := <-s.evCh:
					s.append(ev)
				default:
					s.flush()
					return
				}
			}
		}
	}
}

func (s *Stats) append(ev Event) {
	ev = trimEvent(ev)
	s.evMu.Lock()
	s.events = append(s.events, ev)
	if len(s.events) > eventRingSize {
		s.events = s.events[len(s.events)-eventRingSize:]
	}
	s.evMu.Unlock()
	s.publish(ev)
	if s.rec == nil {
		return
	}
	// ponytail: retry backlog cap; a recorder down for longer than this
	// drops new events (counted as droppedEvents) instead of growing
	// memory forever. Raise it only if the event log must survive longer
	// outages.
	if len(s.pending) >= pendingMax {
		s.dropped.Add(1)
		return
	}
	s.pending = append(s.pending, ev)
}

// Subscribe returns a channel that receives every event appended to the
// feed from now on, plus a function that unregisters it and closes the
// channel. Delivery is best-effort: a subscriber that cannot keep up misses
// events (the dashboard re-reads the ring after a reconnect) instead of
// stalling the collector. The SSE endpoint is the intended consumer.
func (s *Stats) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, subscriberBuffer)
	s.subMu.Lock()
	if s.subs == nil {
		s.subs = make(map[chan Event]struct{})
	}
	s.subs[ch] = struct{}{}
	s.subMu.Unlock()
	return ch, func() {
		s.subMu.Lock()
		if _, ok := s.subs[ch]; ok {
			delete(s.subs, ch)
			close(ch)
		}
		s.subMu.Unlock()
	}
}

// publish fans an event out to the live subscribers. The lock also guards
// against sending on a channel that a disconnecting subscriber just closed.
func (s *Stats) publish(ev Event) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- ev:
		default:
			// Slow consumer: drop this event rather than block the collector.
		}
	}
}

// flush persists the bucket delta since the last flush, the lifetime
// totals and the pending events. Only the Run goroutine calls it, so the
// pending slice needs no lock. On recorder errors the delta baseline and
// the pending batch are kept, so failed writes are retried next flush.
func (s *Stats) flush() {
	if s.rec == nil {
		return
	}
	t := s.totals()
	s.flushMu.Lock()
	delta := subtract(t, s.lastFlush)
	s.flushMu.Unlock()
	var st StoreStat
	if s.storeSampler != nil {
		st = s.storeSampler()
	}
	if err := s.rec.RecordBucket(Bucket{Bucket: time.Now().UTC(), Totals: delta, Store: st}); err != nil {
		s.dbErrs.Add(1)
	} else {
		s.flushMu.Lock()
		s.lastFlush = t
		s.flushMu.Unlock()
	}
	if err := s.rec.SaveTotals(t); err != nil {
		s.dbErrs.Add(1)
	}
	if len(s.pending) == 0 {
		return
	}
	if err := s.rec.AppendEvents(s.pending); err != nil {
		s.dbErrs.Add(1)
	} else {
		s.pending = s.pending[:0]
	}
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

// NeighbourHit records a cache miss satisfied by a neighbouring goproxy,
// so no upstream bandwidth was used. It counts as a local miss (the cache
// was cold) and as a neighbour hit.
func (s *Stats) NeighbourHit(mod, ver, kind string, fetched, served int64, dur time.Duration, verified bool, source string) {
	s.requests.Add(1)
	s.misses.Add(1)
	s.neighbourHits.Add(1)
	s.bytesNeighbour.Add(fetched)
	s.bytesDown.Add(served)
	if verified {
		s.verifications.Add(1)
	}
	s.send(Event{
		Time: time.Now().UTC(), Module: mod, Version: ver, Kind: kind,
		Result: "neighbour", Bytes: fetched, DurMS: dur.Milliseconds(),
		Detail: "from neighbour " + source + "; " + verifiedText(verified),
	})
}

// NeighbourMiss records that the configured neighbour caches had no copy
// and the request fell back to the upstream proxy.
func (s *Stats) NeighbourMiss() {
	s.neighbourMisses.Add(1)
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

// Prewarm records the upstream and neighbour bandwidth an hourly cache
// sync spent and publishes one summary event. A sync is not a client
// request, so the request, hit and miss counters stay untouched.
func (s *Stats) Prewarm(modules, fetched, failed int, upstreamBytes, peerBytes int64) {
	s.bytesUp.Add(upstreamBytes)
	s.bytesNeighbour.Add(peerBytes)
	detail := fmt.Sprintf("fleet sync: %d hottest modules, %d fetched, %s pulled",
		modules, fetched, humanBytes(upstreamBytes+peerBytes))
	if failed > 0 {
		detail += fmt.Sprintf(", %d failed", failed)
	}
	s.send(Event{Time: time.Now().UTC(), Result: "prewarm", Detail: detail})
}

// Maintenance publishes the result of a nightly stats-database window.
func (s *Stats) Maintenance(detail string) {
	s.send(Event{Time: time.Now().UTC(), Result: "maintenance", Detail: detail})
}

func verifiedText(v bool) string {
	if v {
		return "verified against checksum db"
	}
	return "unverified: not in checksum db"
}

// humanBytes renders a byte count for the event feed, where the dashboard
// has no structured byte field to format itself.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// trimEvent keeps hostile request paths from turning the in-memory feed or
// the 90-day SQLite event log into a memory/disk amplifier. The limit is
// above the maximum valid module path length, so real data is untouched.
func trimEvent(ev Event) Event {
	ev.Module = trimText(ev.Module)
	ev.Version = trimText(ev.Version)
	ev.Kind = trimText(ev.Kind)
	ev.Detail = trimText(ev.Detail)
	return ev
}

func trimText(s string) string {
	if len(s) <= maxEventText {
		return s
	}
	return s[:maxEventText] + "..."
}

func (s *Stats) send(ev Event) {
	ev = trimEvent(ev)
	select {
	case s.evCh <- ev:
	default:
		s.dropped.Add(1)
	}
}

// Snapshot returns the current state for the dashboard.
func (s *Stats) Snapshot() Snapshot {
	return s.snapshot(true)
}

// Counters is Snapshot without the event ring: the live push sends events
// individually on the same stream, so copying 512 rows every tick would be
// wasted work.
func (s *Stats) Counters() Snapshot {
	return s.snapshot(false)
}

func (s *Stats) snapshot(withEvents bool) Snapshot {
	t := s.totals()
	var events []Event
	if withEvents {
		s.evMu.Lock()
		events = make([]Event, len(s.events))
		copy(events, s.events)
		s.evMu.Unlock()
	}
	s.tMu.Lock()
	traffic := make([]Traffic, len(s.traffic))
	copy(traffic, s.traffic)
	s.tMu.Unlock()
	if withEvents {
		// Newest first for the feed.
		for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
			events[i], events[j] = events[j], events[i]
		}
		if len(events) > 128 {
			events = events[:128]
		}
	}
	return Snapshot{
		StartedAt:     s.started,
		UptimeSec:     int64(time.Since(s.started).Seconds()),
		Lifetime:      t,
		Session:       subtract(t, s.loaded),
		Traffic:       traffic,
		Events:        events,
		DroppedEvents: s.dropped.Load(),
		PersistErrors: s.dbErrs.Load(),
		Memory:        s.memory(),
		Started:       true,
	}
}

func (s *Stats) totals() Totals {
	return Totals{
		Requests:        s.requests.Load(),
		Hits:            s.hits.Load(),
		Misses:          s.misses.Load(),
		Passes:          s.passes.Load(),
		BytesDown:       s.bytesDown.Load(),
		BytesUp:         s.bytesUp.Load(),
		BytesSaved:      s.bytesSaved.Load(),
		BytesNeighbour:  s.bytesNeighbour.Load(),
		Verifications:   s.verifications.Load(),
		VerifyFailures:  s.verifyFailed.Load(),
		NeighbourHits:   s.neighbourHits.Load(),
		NeighbourMisses: s.neighbourMisses.Load(),
	}
}

func subtract(a, b Totals) Totals {
	return Totals{
		Requests:        a.Requests - b.Requests,
		Hits:            a.Hits - b.Hits,
		Misses:          a.Misses - b.Misses,
		Passes:          a.Passes - b.Passes,
		BytesDown:       a.BytesDown - b.BytesDown,
		BytesUp:         a.BytesUp - b.BytesUp,
		BytesSaved:      a.BytesSaved - b.BytesSaved,
		BytesNeighbour:  a.BytesNeighbour - b.BytesNeighbour,
		Verifications:   a.Verifications - b.Verifications,
		VerifyFailures:  a.VerifyFailures - b.VerifyFailures,
		NeighbourHits:   a.NeighbourHits - b.NeighbourHits,
		NeighbourMisses: a.NeighbourMisses - b.NeighbourMisses,
	}
}
