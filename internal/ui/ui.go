// Package ui serves the embedded operations dashboard and its JSON APIs.
package ui

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"paepcke.de/goproxy/internal/history"
	"paepcke.de/goproxy/internal/stats"
	"paepcke.de/goproxy/internal/store"
)

//go:embed static
var staticFS embed.FS

const (
	stateInterval     = 2 * time.Second  // live dashboard push cadence
	historyInterval   = 30 * time.Second // long-term series refresh
	keepaliveInterval = 25 * time.Second
)

// historyRange is one selectable dashboard history window. Aggregates
// minute buckets into slots of res, keeping each response compact.
type historyRange struct {
	span time.Duration
	res  time.Duration
}

var historyRanges = map[string]historyRange{
	"24h": {span: 24 * time.Hour, res: 5 * time.Minute},
	"7d":  {span: 7 * 24 * time.Hour, res: time.Hour},
	"30d": {span: 30 * 24 * time.Hour, res: 6 * time.Hour},
	"1y":  {span: 365 * 24 * time.Hour, res: 24 * time.Hour},
	"all": {span: 0, res: 24 * time.Hour}, // from the first recorded bucket
}

// dashboardPayload is the one-shot dashboard state for scripts and
// health probes; the dashboard page itself only consumes the SSE stream.
type dashboardPayload struct {
	Version string          `json:"version"`
	Stats   stats.Snapshot  `json:"stats"`
	Store   store.Summary   `json:"store"`
	Modules []store.ModStat `json:"modules"`
}

// statePayload is the cache half of a live dashboard push: the store
// summary, the module table and the running version.
type statePayload struct {
	Version string          `json:"version"`
	Store   store.Summary   `json:"store"`
	Modules []store.ModStat `json:"modules"`
}

// historyPayload is one aggregated long-term series.
type historyPayload struct {
	Range         string          `json:"range"`
	From          time.Time       `json:"from"`
	To            time.Time       `json:"to"`
	ResolutionSec int64           `json:"resolutionSec"`
	Points        []history.Point `json:"points"`
	Summary       history.Summary `json:"summary"`
}

// Handler serves the dashboard page, its assets, /api/dashboard,
// /api/history and the live /api/events stream.
type Handler struct {
	stats   *stats.Stats
	store   *store.Store
	hist    *history.DB
	version string
}

// New creates the dashboard handler.
func New(m *stats.Stats, s *store.Store, h *history.DB, version string) *Handler {
	return &Handler{stats: m, store: s, hist: h, version: version}
}

// ServeHTTP routes /, /static/*, /api/dashboard, /api/history and
// /api/events.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/dashboard":
		h.dashboard(w, r)
	case r.URL.Path == "/api/history":
		h.history(w, r)
	case r.URL.Path == "/api/events":
		h.events(w, r)
	case r.URL.Path == "/" || r.URL.Path == "/index.html":
		h.index(w, r)
	case strings.HasPrefix(r.URL.Path, "/static/"):
		sub, err := fs.Sub(staticFS, "static")
		if err != nil {
			http.Error(w, "goproxy: ui", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.StripPrefix("/static/", http.FileServer(http.FS(sub))).ServeHTTP(w, r)
	default:
		http.NotFound(w, r)
	}
}

// state collects the full dashboard state in one consistent read.
func (h *Handler) state() dashboardPayload {
	return dashboardPayload{
		Version: h.version,
		Stats:   h.stats.Snapshot(),
		Store:   h.store.Summary(),
		Modules: h.store.Modules(),
	}
}

// dashboard serves the full state as JSON for scripts and probes. The
// dashboard page never calls it: every value it shows arrives over SSE.
func (h *Handler) dashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(h.state()); err != nil {
		// Headers are already sent; nothing more to do.
		_ = err
	}
}

// historyData aggregates one range into minute buckets of res. The caller
// must have validated the range name against historyRanges.
func (h *Handler) historyData(name string) (historyPayload, error) {
	spec := historyRanges[name]
	res := historyPayload{Range: name, ResolutionSec: int64(spec.res.Seconds())}

	now := time.Now().UTC()
	// Buckets are minute-aligned and the range end is exclusive, so the
	// in-progress minute only counts once it is rounded up to its end.
	to := now.Truncate(time.Minute).Add(time.Minute)
	from := now.Add(-spec.span)
	if spec.span == 0 {
		earliest, _, err := h.hist.Bounds()
		if err != nil {
			return historyPayload{}, err
		}
		if earliest.IsZero() {
			earliest = now.Add(-24 * time.Hour)
		}
		from = earliest
	}
	res.From, res.To = from, to

	points, err := h.hist.Series(from, to, spec.res)
	if err != nil {
		return historyPayload{}, err
	}
	res.Points = points
	if res.Summary, err = h.hist.RangeSummary(from, to); err != nil {
		return historyPayload{}, err
	}
	return res, nil
}

// history serves the long-term stats series: ?range=24h|7d|30d|1y|all.
func (h *Handler) history(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("range")
	if _, ok := historyRanges[name]; !ok {
		http.Error(w, "goproxy: unknown history range", http.StatusBadRequest)
		return
	}
	res, err := h.historyData(name)
	if err != nil {
		http.Error(w, "goproxy: history", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(res); err != nil {
		_ = err
	}
}

// statsState renders the live counters without the event ring: events
// travel as individual messages on the same stream.
func (h *Handler) statsState() ([]byte, error) {
	snap := h.stats.Snapshot()
	snap.Events = nil
	return json.Marshal(snap)
}

// storeState renders the cache summary and module table.
func (h *Handler) storeState() ([]byte, error) {
	return json.Marshal(statePayload{
		Version: h.version,
		Store:   h.store.Summary(),
		Modules: h.store.Modules(),
	})
}

// events streams the whole dashboard as Server-Sent Events: the recent
// event ring once on connect, one message per new event, the live
// counters and cache table every stateInterval (only when they changed),
// the long-term series every historyInterval for ?range=, and a tick that
// ages timestamps while keeping idle connections open.
func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "goproxy: streaming unsupported", http.StatusInternalServerError)
		return
	}
	rangeName := r.URL.Query().Get("range")
	if rangeName == "" {
		rangeName = "24h"
	}
	_, pushHistory := historyRanges[rangeName]

	ch, unsubscribe := h.stats.Subscribe()
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	// Disable proxy buffering (caddy, nginx) so events arrive immediately.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(w, ": connected\n\n"); err != nil {
		return
	}
	fl.Flush()

	// Seed a fresh dashboard: the event ring, then counters and cache,
	// then the selected history range. Subscribing happens first, so no
	// event can slip between the seed and the stream; an event may thus
	// appear twice (seed + message), which the dashboard deduplicates.
	seedEvents, err := json.Marshal(h.stats.Snapshot().Events)
	if err != nil || writeSSE(w, "events", seedEvents) != nil {
		return
	}
	lastStore, err := h.storeState()
	if err != nil || writeSSE(w, "store", lastStore) != nil {
		return
	}
	lastStats, err := h.statsState()
	if err != nil || writeSSE(w, "stats", lastStats) != nil {
		return
	}
	var lastHistory []byte
	if pushHistory {
		if hp, err := h.historyData(rangeName); err == nil {
			if data, err := json.Marshal(hp); err == nil {
				if writeSSE(w, "history", data) != nil {
					return
				}
				lastHistory = data
			}
		}
	}
	fl.Flush()

	stateTick := time.NewTicker(stateInterval)
	defer stateTick.Stop()
	historyTick := time.NewTicker(historyInterval)
	defer historyTick.Stop()
	keepalive := time.NewTicker(keepaliveInterval)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			// A tick keeps idle connections open and lets the page age its
			// timestamps without a client-side timer or another request.
			if writeSSE(w, "tick", []byte("{}")) != nil {
				return
			}
			fl.Flush()
		case <-stateTick.C:
			sent := false
			if data, err := h.statsState(); err == nil && !bytes.Equal(data, lastStats) {
				lastStats = data
				if writeSSE(w, "stats", data) != nil {
					return
				}
				sent = true
			}
			if data, err := h.storeState(); err == nil && !bytes.Equal(data, lastStore) {
				lastStore = data
				if writeSSE(w, "store", data) != nil {
					return
				}
				sent = true
			}
			if sent {
				fl.Flush()
			}
		case <-historyTick.C:
			if !pushHistory {
				continue
			}
			hp, err := h.historyData(rangeName)
			if err != nil {
				continue
			}
			data, err := json.Marshal(hp)
			if err != nil || bytes.Equal(data, lastHistory) {
				continue
			}
			lastHistory = data
			if writeSSE(w, "history", data) != nil {
				return
			}
			fl.Flush()
		case ev, open := <-ch:
			if !open {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if writeSSE(w, "event", data) != nil {
				return
			}
			fl.Flush()
		}
	}
}

// writeSSE emits one Server-Sent Event message.
func writeSSE(w io.Writer, event string, data []byte) error {
	_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	return err
}

func (h *Handler) index(w http.ResponseWriter, r *http.Request) {
	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "goproxy: ui", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(data)
}
