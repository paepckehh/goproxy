package ui

import (
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
				Requests int64 `json:"requests"`
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
