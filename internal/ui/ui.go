// Package ui serves the embedded operations dashboard and its JSON APIs.
package ui

import (
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

// Handler serves the dashboard page, its assets, /api/dashboard and
// /api/history.
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

// dashboard aggregates stats, store summary and module table in one poll.
func (h *Handler) dashboard(w http.ResponseWriter, r *http.Request) {
	type payload struct {
		Version string          `json:"version"`
		Stats   stats.Snapshot  `json:"stats"`
		Store   store.Summary   `json:"store"`
		Modules []store.ModStat `json:"modules"`
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	if err := enc.Encode(payload{
		Version: h.version,
		Stats:   h.stats.Snapshot(),
		Store:   h.store.Summary(),
		Modules: h.store.Modules(),
	}); err != nil {
		// Headers are already sent; nothing more to do.
		_ = err
	}
}

// events streams the live event feed as Server-Sent Events, so the
// dashboard appends new events instead of re-rendering the whole list on
// every poll. Each message carries one stats.Event as JSON under the
// "event" event name; comment lines keep idle connections open.
func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "goproxy: streaming unsupported", http.StatusInternalServerError)
		return
	}
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

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
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
			if _, err := fmt.Fprintf(w, "event: event\ndata: %s\n\n", data); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// history serves the long-term stats series: ?range=24h|7d|30d|1y|all.
func (h *Handler) history(w http.ResponseWriter, r *http.Request) {
	spec, ok := historyRanges[r.URL.Query().Get("range")]
	if !ok {
		http.Error(w, "goproxy: unknown history range", http.StatusBadRequest)
		return
	}
	type payload struct {
		Range         string          `json:"range"`
		From          time.Time       `json:"from"`
		To            time.Time       `json:"to"`
		ResolutionSec int64           `json:"resolutionSec"`
		Points        []history.Point `json:"points"`
		Summary       history.Summary `json:"summary"`
	}
	res := payload{Range: r.URL.Query().Get("range"), ResolutionSec: int64(spec.res.Seconds())}

	to := time.Now().UTC()
	from := to.Add(-spec.span)
	if spec.span == 0 {
		earliest, _, err := h.hist.Bounds()
		if err != nil {
			http.Error(w, "goproxy: history", http.StatusInternalServerError)
			return
		}
		if earliest.IsZero() {
			earliest = to.Add(-24 * time.Hour)
		}
		from = earliest
	}
	res.From, res.To = from, to

	points, err := h.hist.Series(from, to, spec.res)
	if err != nil {
		http.Error(w, "goproxy: history", http.StatusInternalServerError)
		return
	}
	res.Points = points
	if res.Summary, err = h.hist.RangeSummary(from, to); err != nil {
		http.Error(w, "goproxy: history", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(res); err != nil {
		_ = err
	}
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
