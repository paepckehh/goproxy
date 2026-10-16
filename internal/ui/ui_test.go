package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	return New(stats.New(""), st, "v0.0.0-test")
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
