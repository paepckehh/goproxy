package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveRoot(t *testing.T) {
	for _, tc := range []struct {
		name string
		flag string
		env  string
		want string
	}{
		{name: "default", want: "cache"},
		{name: "flag wins over env", flag: "/disk2/go/proxy", env: "/other/cache", want: "/disk2/go/proxy"},
		{name: "env used", env: "/disk2/go/proxy", want: "/disk2/go/proxy"},
		{name: "relative env allowed", env: "gocache", want: "gocache"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CACHE_ROOT", tc.env)
			if got := resolveRoot(tc.flag); got != tc.want {
				t.Fatalf("resolveRoot(%q) = %q, want %q", tc.flag, got, tc.want)
			}
		})
	}
}

func TestResolveStatsDB(t *testing.T) {
	stateDir := t.TempDir()
	fileAsStateDir := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(fileAsStateDir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		root     string
		stateDir string
		want     string
	}{
		{name: "state dir wins", root: "/cache", stateDir: stateDir, want: filepath.Join(stateDir, "stats.db")},
		{name: "missing state dir falls back to cache root", root: "/cache",
			stateDir: filepath.Join(t.TempDir(), "does", "not", "exist"), want: "/cache/stats.db"},
		{name: "state dir file not dir falls back", root: "/cache",
			stateDir: fileAsStateDir, want: "/cache/stats.db"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveStatsDB(tc.root, tc.stateDir); got != tc.want {
				t.Fatalf("resolveStatsDB(%q, %q) = %q, want %q", tc.root, tc.stateDir, got, tc.want)
			}
		})
	}
}

func TestLogLevel(t *testing.T) {
	if got := logLevel(false); got != slog.LevelInfo {
		t.Errorf("logLevel(false) = %v, want Info", got)
	}
	if got := logLevel(true); got != slog.LevelDebug {
		t.Errorf("logLevel(true) = %v, want Debug", got)
	}
}

func TestAccessLogHonorsLevel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		level slog.Level
		want  string
	}{
		{name: "default hides access lines", level: slog.LevelInfo, want: ""},
		{name: "DEBUG shows access lines", level: slog.LevelDebug, want: "access"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: tc.level}))
			h := withAccessLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusTeapot)
			}), log)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/example.com/m/@v/list", nil))
			got := buf.String()
			if tc.want == "" {
				if got != "" {
					t.Fatalf("logs = %q, want no access line", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) || !strings.Contains(got, "418") {
				t.Fatalf("logs = %q, want the access line with status", got)
			}
		})
	}
}

// The SSE dashboard streams through the access-log wrapper; it must still
// expose http.Flusher.
func TestAccessLogKeepsFlusher(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := withAccessLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Error("wrapped writer lost http.Flusher")
		}
	}), log)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/events", nil))
}
