package ui

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"paepcke.de/goproxy/internal/history"
	"paepcke.de/goproxy/internal/stats"
	"paepcke.de/goproxy/internal/store"
)

func newTestUI(t *testing.T) *Handler {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutBytes("example.com/m", "v1.0.0", store.KindMod, []byte("module example.com/m\n"), "h1:x=", true); err != nil {
		t.Fatal(err)
	}
	hist, err := history.Open(filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hist.Close() })
	return New(stats.New(nil), st, hist, "v0.0.0-test")
}

func TestUI_IndexServesDashboard(t *testing.T) {
	t.Parallel()
	h := newTestUI(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "goproxy") || !strings.Contains(body, "/static/app.js") {
		t.Error("dashboard html incomplete")
	}
	if !strings.Contains(body, "chart-history") {
		t.Error("dashboard html missing the long-term history section")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content type = %q", ct)
	}
}

func TestUI_DashboardAPI(t *testing.T) {
	t.Parallel()
	h := newTestUI(t)
	h.stats.NeighbourHit("example.com/m", "v1.0.0", "zip", 2048, 2048, time.Millisecond, true, "peer:9090")

	req := httptest.NewRequest(http.MethodGet, "/api/dashboard", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content type = %q", ct)
	}

	var payload struct {
		Version string `json:"version"`
		Stats   struct {
			Lifetime struct {
				Requests       int64 `json:"requests"`
				NeighbourHits  int64 `json:"neighbourHits"`
				NeighbourMiss  int64 `json:"neighbourMisses"`
				BytesNeighbour int64 `json:"bytesNeighbour"`
			} `json:"lifetime"`
		} `json:"stats"`
		Store struct {
			Modules int   `json:"modules"`
			Size    int64 `json:"sizeBytes"`
		} `json:"store"`
		Modules []store.ModStat `json:"modules"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Version != "v0.0.0-test" {
		t.Errorf("version = %q", payload.Version)
	}
	if payload.Store.Modules != 1 {
		t.Errorf("store modules = %d, want 1", payload.Store.Modules)
	}
	if len(payload.Modules) != 1 || payload.Modules[0].Path != "example.com/m" {
		t.Errorf("modules = %+v", payload.Modules)
	}
	if !payload.Modules[0].Verified {
		t.Error("module not reported as verified")
	}
	if payload.Stats.Lifetime.NeighbourHits != 1 || payload.Stats.Lifetime.BytesNeighbour != 2048 {
		t.Errorf("neighbour stats = %+v, want 1 hit / 2048 bytes", payload.Stats.Lifetime)
	}
}

func TestUI_HistoryAPI(t *testing.T) {
	t.Parallel()
	h := newTestUI(t)

	// Some long-term history to serve.
	now := time.Now().UTC()
	if err := h.hist.RecordBucket(stats.Bucket{
		Bucket: now.Truncate(time.Minute),
		Totals: stats.Totals{Requests: 5, Hits: 4, Misses: 1, BytesSaved: 4096},
		Store:  stats.StoreStat{Bytes: 2048},
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/history?range=24h", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content type = %q", ct)
	}
	var payload struct {
		Range  string `json:"range"`
		Points []struct {
			Requests int64 `json:"requests"`
			Hits     int64 `json:"hits"`
		} `json:"points"`
		Summary struct {
			Requests   int64 `json:"requests"`
			Hits       int64 `json:"hits"`
			BytesSaved int64 `json:"bytesSaved"`
			StoreStart int64 `json:"storeStart"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Range != "24h" {
		t.Errorf("range = %q", payload.Range)
	}
	if len(payload.Points) != 1 || payload.Points[0].Requests != 5 {
		t.Errorf("points = %+v", payload.Points)
	}
	if payload.Summary.Requests != 5 || payload.Summary.Hits != 4 || payload.Summary.BytesSaved != 4096 {
		t.Errorf("summary = %+v", payload.Summary)
	}
	if payload.Summary.StoreStart != 2048 {
		t.Errorf("summary storeStart = %d, want 2048", payload.Summary.StoreStart)
	}
}

func TestUI_HistoryAPIIncludesNeighbourCounters(t *testing.T) {
	t.Parallel()
	h := newTestUI(t)

	now := time.Now().UTC()
	if err := h.hist.RecordBucket(stats.Bucket{
		Bucket: now.Truncate(time.Minute),
		Totals: stats.Totals{Requests: 2, Misses: 1, NeighbourHits: 1,
			NeighbourMisses: 1, BytesNeighbour: 512},
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/history?range=24h", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var payload struct {
		Points []struct {
			NeighbourHits   int64 `json:"neighbourHits"`
			NeighbourMisses int64 `json:"neighbourMisses"`
			BytesNeighbour  int64 `json:"bytesNeighbour"`
		} `json:"points"`
		Summary struct {
			NeighbourHits   int64 `json:"neighbourHits"`
			NeighbourMisses int64 `json:"neighbourMisses"`
			BytesNeighbour  int64 `json:"bytesNeighbour"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Points) != 1 {
		t.Fatalf("points = %d, want 1", len(payload.Points))
	}
	if p := payload.Points[0]; p.NeighbourHits != 1 || p.NeighbourMisses != 1 || p.BytesNeighbour != 512 {
		t.Errorf("point = %+v", p)
	}
	if s := payload.Summary; s.NeighbourHits != 1 || s.NeighbourMisses != 1 || s.BytesNeighbour != 512 {
		t.Errorf("summary = %+v", s)
	}
}

func TestUI_HistoryAPIAllRange(t *testing.T) {
	t.Parallel()
	h := newTestUI(t)

	// "all" works on an empty database without erroring.
	req := httptest.NewRequest(http.MethodGet, "/api/history?range=all", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var payload struct {
		Points []json.RawMessage `json:"points"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Points) != 0 {
		t.Errorf("points = %d, want 0 on an empty database", len(payload.Points))
	}
}

func TestUI_HistoryAPIRejectsUnknownRange(t *testing.T) {
	t.Parallel()
	h := newTestUI(t)

	req := httptest.NewRequest(http.MethodGet, "/api/history?range=nope", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestUI_StaticAssets(t *testing.T) {
	t.Parallel()
	h := newTestUI(t)

	for path, want := range map[string]string{
		"/static/style.css": "text/css",
		"/static/app.js":    "javascript",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s status = %d", path, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, want) {
			t.Errorf("%s content type = %q, want %q", path, ct, want)
		}
		if path == "/static/app.js" && !strings.Contains(rec.Body.String(), "neighbourHits") {
			t.Error("dashboard script does not render the neighbour cache stats")
		}
	}
}

func TestUI_UnknownPathNotFound(t *testing.T) {
	t.Parallel()
	h := newTestUI(t)
	req := httptest.NewRequest(http.MethodGet, "/nope", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestUI_EventsSSEStreamsNewEvents(t *testing.T) {
	t.Parallel()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hist, err := history.Open(filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer hist.Close()

	m := stats.New(nil)
	runCtx, stop := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		m.Run(runCtx)
		close(runDone)
	}()
	defer func() {
		stop()
		<-runDone
	}()

	reader := openSSE(t, New(m, st, hist, "v0.0.0-test"), "")

	// A fresh stream seeds the whole dashboard before live traffic.
	for _, want := range []string{"events", "store", "stats", "history"} {
		if name, _ := readSSE(t, reader); name != want {
			t.Fatalf("seed message = %q, want %q", name, want)
		}
	}

	m.Hit("example.com/m", "v1.0.0", "zip", 42, time.Millisecond)

	name, payload := readSSE(t, reader)
	if name != "event" {
		t.Fatalf("message = %q, want event", name)
	}
	var ev stats.Event
	if err := json.Unmarshal(payload, &ev); err != nil {
		t.Fatalf("event payload %q: %v", payload, err)
	}
	if ev.Result != "hit" || ev.Module != "example.com/m" || ev.Bytes != 42 {
		t.Errorf("event = %+v", ev)
	}
}

// openSSE connects to /api/events and consumes the "connected" comment,
// leaving the reader at the first seeded event.
func openSSE(t *testing.T, h *Handler, query string) *bufio.Reader {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	reqCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, srv.URL+"/api/events"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q", ct)
	}

	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, ": connected") {
		t.Fatalf("first line = %q", line)
	}
	return reader
}

// readSSE returns the next named message (comments and keepalives are
// skipped) and its JSON payload.
func readSSE(t *testing.T, r *bufio.Reader) (string, []byte) {
	t.Helper()
	var event string
	var data []byte
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if event != "" {
				return event, data
			}
		case strings.HasPrefix(line, ":"):
			// comment or keepalive
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = []byte(strings.TrimPrefix(line, "data: "))
		}
	}
}

// The seed carries the cache table, and cache changes are pushed without
// any polling.
func TestUI_EventsSSEPushesCacheUpdates(t *testing.T) {
	t.Parallel()
	h := newTestUI(t)
	reader := openSSE(t, h, "?range=24h")

	seeded := map[string]bool{}
	for len(seeded) < 4 {
		name, payload := readSSE(t, reader)
		seeded[name] = true
		if name != "store" {
			continue
		}
		var state struct {
			Version string          `json:"version"`
			Modules []store.ModStat `json:"modules"`
		}
		if err := json.Unmarshal(payload, &state); err != nil {
			t.Fatalf("store payload %q: %v", payload, err)
		}
		if state.Version != "v0.0.0-test" {
			t.Errorf("version = %q", state.Version)
		}
		if len(state.Modules) != 1 || state.Modules[0].Path != "example.com/m" {
			t.Errorf("seed modules = %+v", state.Modules)
		}
	}

	if _, err := h.store.PutBytes("example.com/new", "v1.0.0", store.KindMod,
		[]byte("module example.com/new\n"), "h1:y=", true); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		name, payload := readSSE(t, reader)
		if name != "store" {
			continue
		}
		var state struct {
			Modules []store.ModStat `json:"modules"`
		}
		if err := json.Unmarshal(payload, &state); err != nil {
			t.Fatalf("store payload %q: %v", payload, err)
		}
		for _, m := range state.Modules {
			if m.Path == "example.com/new" {
				return
			}
		}
	}
	t.Fatal("no pushed cache update carrying the new module")
}
