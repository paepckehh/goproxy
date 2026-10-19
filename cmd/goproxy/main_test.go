package main

import (
	"os"
	"path/filepath"
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
