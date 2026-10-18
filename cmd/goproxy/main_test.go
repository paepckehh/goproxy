package main

import (
	"path/filepath"
	"testing"
)

func TestResolveStatsDB(t *testing.T) {
	for _, tc := range []struct {
		name    string
		flag    string
		env     string
		root    string
		want    string
		wantErr bool
	}{
		{name: "default in root", root: "/cache", want: filepath.Join("/cache", "stats.db")},
		{name: "flag wins over env", flag: "/var/lib/goproxy/stats.db", env: "/other/db.sqlite", want: "/var/lib/goproxy/stats.db"},
		{name: "env used", env: "/var/lib/goproxy/stats.db", want: "/var/lib/goproxy/stats.db"},
		{name: "relative env rejected", env: "stats.db", wantErr: true},
		{name: "relative flag rejected", flag: "db/stats.db", wantErr: true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("STATS_DB", tc.env)
			got, err := resolveStatsDB(tc.flag, tc.root)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveStatsDB(%q, %q) = %q, want error", tc.flag, tc.root, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveStatsDB(%q, %q) error: %v", tc.flag, tc.root, err)
			}
			if got != tc.want {
				t.Fatalf("resolveStatsDB(%q, %q) = %q, want %q", tc.flag, tc.root, got, tc.want)
			}
		})
	}
}
