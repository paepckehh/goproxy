// Package ui serves the embedded operations dashboard and its JSON API.
package ui

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"

	"paepcke.de/goproxy/internal/stats"
	"paepcke.de/goproxy/internal/store"
)

//go:embed static
var staticFS embed.FS

// Handler serves the dashboard page, its assets, and /api/dashboard.
type Handler struct {
	stats   *stats.Stats
	store   *store.Store
	version string
}

// New creates the dashboard handler.
func New(m *stats.Stats, s *store.Store, version string) *Handler {
	return &Handler{stats: m, store: s, version: version}
}

// ServeHTTP routes /, /static/* and /api/dashboard.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/dashboard":
		h.dashboard(w, r)
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
