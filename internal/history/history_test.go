package history

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"paepcke.de/goproxy/internal/stats"
	"paepcke.de/goproxy/internal/store"
)

func open(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestHistory_TotalsRoundtrip(t *testing.T) {
	t.Parallel()
	db := open(t)

	want := stats.Totals{Requests: 42, Hits: 40, Misses: 1, Passes: 1,
		BytesDown: 1234, BytesUp: 567, BytesSaved: 800, BytesNeighbour: 321,
		Verifications: 1, VerifyFailures: 2, NeighbourHits: 7, NeighbourMisses: 3}
	if err := db.SaveTotals(want); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveTotals(stats.Totals{Hits: 3}); err != nil { // upsert, not insert
		t.Fatal(err)
	}
	got, err := db.LoadTotals()
	if err != nil {
		t.Fatal(err)
	}
	if got != (stats.Totals{Hits: 3}) {
		t.Errorf("totals = %+v, want overwrite with hits 3", got)
	}
}

func TestHistory_ModuleUsageRoundtrip(t *testing.T) {
	t.Parallel()
	db := open(t)

	want := map[string]store.ModuleUsage{
		"a.example/x": {Requests: 2, Served: 150, LastUsed: time.Unix(1700000000, 0).UTC()},
		"b.example/y": {Requests: 7, Served: 4096},
	}
	if err := db.SaveModuleUsage(want); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveModuleUsage(map[string]store.ModuleUsage{
		"a.example/x": {Requests: 3, Served: 200, LastUsed: time.Unix(1700000100, 0).UTC()},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := db.LoadModuleUsage()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("usage = %+v, want 2 modules", got)
	}
	if u := got["a.example/x"]; u.Requests != 3 || u.Served != 200 ||
		!u.LastUsed.Equal(time.Unix(1700000100, 0)) {
		t.Errorf("usage a = %+v, want upserted counters", u)
	}
	if u := got["b.example/y"]; u.Requests != 7 || u.Served != 4096 || !u.LastUsed.IsZero() {
		t.Errorf("usage b = %+v, want untouched counters", u)
	}
}

func TestHistory_BucketUpsertMerge(t *testing.T) {
	t.Parallel()
	db := open(t)

	minute := time.Now().UTC().Truncate(time.Minute)
	first := stats.Bucket{
		Bucket: minute,
		Totals: stats.Totals{Requests: 3, Hits: 2, Misses: 1, BytesSaved: 100,
			BytesNeighbour: 40, NeighbourHits: 1, NeighbourMisses: 2},
		Store: stats.StoreStat{Bytes: 1000, Modules: 10},
	}
	second := stats.Bucket{
		Bucket: minute.Add(20 * time.Second), // same wall-clock minute
		Totals: stats.Totals{Requests: 2, Hits: 2, BytesSaved: 50,
			BytesNeighbour: 10, NeighbourHits: 1},
		Store: stats.StoreStat{Bytes: 2000, Modules: 12},
	}
	if err := db.RecordBucket(first); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordBucket(second); err != nil {
		t.Fatal(err)
	}

	pts, err := db.Series(minute.Add(-time.Minute), minute.Add(time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 1 {
		t.Fatalf("points = %d, want 1", len(pts))
	}
	p := pts[0]
	if p.Requests != 5 || p.Hits != 4 || p.Misses != 1 || p.BytesSaved != 150 {
		t.Errorf("merged bucket = %+v, want requests 5 hits 4 misses 1 saved 150", p)
	}
	// Deltas merge; store snapshots overwrite (last sample wins).
	if p.StoreBytes != 2000 || p.StoreModules != 12 {
		t.Errorf("store snapshot = %d/%d, want 2000/12 (last wins)", p.StoreBytes, p.StoreModules)
	}
}

func TestHistory_EventsAndRecent(t *testing.T) {
	t.Parallel()
	db := open(t)

	base := time.Now().UTC()
	evs := []stats.Event{
		{Time: base.Add(-2 * time.Minute), Result: "hit", Module: "m", Version: "v1.0.0", Bytes: 10},
		{Time: base.Add(-1 * time.Minute), Result: "miss", Module: "m", Version: "v1.1.0", Detail: "unverified: not in checksum db"},
	}
	if err := db.AppendEvents(evs); err != nil {
		t.Fatal(err)
	}
	got, err := db.RecentEvents(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("events = %d, want 2", len(got))
	}
	if got[0].Result != "miss" || got[1].Result != "hit" {
		t.Errorf("order = %q, %q, want newest first", got[0].Result, got[1].Result)
	}
	if got[0].Module != "m" || got[0].Version != "v1.1.0" || got[0].Detail == "" {
		t.Errorf("event fields lost: %+v", got[0])
	}

	limited, err := db.RecentEvents(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 || limited[0].Result != "miss" {
		t.Errorf("limited = %+v", limited)
	}
}

func TestHistory_SeriesAggregation(t *testing.T) {
	t.Parallel()
	db := open(t)

	base := time.Now().UTC().Truncate(time.Hour)
	// Two hours of buckets, plus one bucket outside the queried range.
	for h := range 3 {
		for m := range 2 {
			b := stats.Bucket{
				Bucket: base.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute),
				Totals: stats.Totals{Requests: 1, Hits: 1, BytesSaved: 10,
					BytesNeighbour: 5, NeighbourHits: 1},
				Store: stats.StoreStat{Bytes: int64(h+1) * 100},
			}
			if err := db.RecordBucket(b); err != nil {
				t.Fatal(err)
			}
		}
	}

	pts, err := db.Series(base, base.Add(2*time.Hour), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 2 {
		t.Fatalf("points = %d, want 2 hourly slots", len(pts))
	}
	for i, p := range pts {
		if p.Requests != 2 || p.Hits != 2 || p.BytesSaved != 20 {
			t.Errorf("point %d = %+v, want 2 requests/hits, 20 saved", i, p)
		}
		if p.BytesNeighbour != 10 || p.NeighbourHits != 2 {
			t.Errorf("point %d neighbour = %d bytes / %d hits, want 10/2",
				i, p.BytesNeighbour, p.NeighbourHits)
		}
		if !p.Time.Equal(base.Add(time.Duration(i) * time.Hour)) {
			t.Errorf("point %d time = %v, want %v", i, p.Time, base.Add(time.Duration(i)*time.Hour))
		}
	}
	if pts[0].StoreBytes != 100 || pts[1].StoreBytes != 200 {
		t.Errorf("store bytes = %d, %d, want 100, 200", pts[0].StoreBytes, pts[1].StoreBytes)
	}

	// Empty range yields no points, not an error.
	pts, err = db.Series(base.Add(-24*time.Hour), base.Add(-23*time.Hour), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 0 {
		t.Errorf("empty range points = %d, want 0", len(pts))
	}
}

func TestHistory_RangeSummaryAndBounds(t *testing.T) {
	t.Parallel()
	db := open(t)

	base := time.Now().UTC().Truncate(time.Hour)
	for m := range 4 {
		b := stats.Bucket{
			Bucket: base.Add(time.Duration(m) * time.Minute),
			Totals: stats.Totals{Requests: 2, Hits: 1, Misses: 1, BytesSaved: 5,
				BytesNeighbour: 7, NeighbourHits: 1, NeighbourMisses: 1},
			Store: stats.StoreStat{Bytes: 100 + int64(m)*10},
		}
		if err := db.RecordBucket(b); err != nil {
			t.Fatal(err)
		}
	}

	s, err := db.RangeSummary(base, base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if s.Requests != 8 || s.Hits != 4 || s.Misses != 4 || s.BytesSaved != 20 {
		t.Errorf("summary = %+v, want 8 requests, 4 hits/misses, 20 saved", s)
	}
	if s.BytesNeighbour != 28 || s.NeighbourHits != 4 || s.NeighbourMisses != 4 {
		t.Errorf("summary neighbour = %d bytes / %d hits / %d misses, want 28/4/4",
			s.BytesNeighbour, s.NeighbourHits, s.NeighbourMisses)
	}
	if s.StoreStart != 100 || s.StoreEnd != 130 {
		t.Errorf("store %d -> %d, want 100 -> 130", s.StoreStart, s.StoreEnd)
	}

	from, to, err := db.Bounds()
	if err != nil {
		t.Fatal(err)
	}
	if !from.Equal(base) {
		t.Errorf("bounds from = %v, want %v", from, base)
	}
	if !to.Equal(base.Add(3 * time.Minute)) {
		t.Errorf("bounds to = %v, want %v", to, base.Add(3*time.Minute))
	}
}

func TestHistory_RangeSummaryGrowthFromEmptyCache(t *testing.T) {
	t.Parallel()
	db := open(t)
	base := time.Now().UTC().Truncate(time.Minute)
	if err := db.RecordBucket(stats.Bucket{
		Bucket: base,
		Totals: stats.Totals{Hits: 1},
		Store:  stats.StoreStat{Bytes: 0},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordBucket(stats.Bucket{
		Bucket: base.Add(time.Minute),
		Totals: stats.Totals{Hits: 1},
		Store:  stats.StoreStat{Bytes: 100},
	}); err != nil {
		t.Fatal(err)
	}
	s, err := db.RangeSummary(base, base.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if s.StoreStart != 0 || s.StoreEnd != 100 {
		t.Fatalf("store range = %d -> %d, want 0 -> 100", s.StoreStart, s.StoreEnd)
	}
}

func TestHistory_ReopenPersistsEverything(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "stats.db")

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	want := stats.Totals{Requests: 7, Hits: 5, Misses: 2, BytesSaved: 500}
	if err := db.SaveTotals(want); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordBucket(stats.Bucket{Bucket: time.Now().UTC(), Totals: stats.Totals{Hits: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := db.AppendEvents([]stats.Event{{Time: time.Now().UTC(), Result: "hit"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := db.LoadTotals()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("totals after reopen = %+v, want %+v", got, want)
	}
	pts, err := db.Series(time.Now().Add(-time.Minute), time.Now().Add(time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 1 || pts[0].Hits != 1 {
		t.Errorf("buckets after reopen = %+v", pts)
	}
	evs, err := db.RecentEvents(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Result != "hit" {
		t.Errorf("events after reopen = %+v", evs)
	}
}

func TestHistory_PruneDeletesOldEvents(t *testing.T) {
	t.Parallel()
	db := open(t)

	now := time.Now().UTC()
	old := stats.Event{Time: now.Add(-eventRetention - time.Hour), Result: "hit"}
	fresh := stats.Event{Time: now, Result: "miss"}
	if err := db.AppendEvents([]stats.Event{old, fresh}); err != nil {
		t.Fatal(err)
	}
	db.prune(now)

	evs, err := db.RecentEvents(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Result != "miss" {
		t.Errorf("events after prune = %+v, want only the fresh miss", evs)
	}
}

func TestHistory_RunStopsOnCancel(t *testing.T) {
	t.Parallel()
	db := open(t)
	db.pruneInterval = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { db.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
}

func TestHistory_ImportLegacyJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db := open(t)
	legacy := filepath.Join(dir, "stats.json")

	// No legacy file: no import, no error.
	if imported, err := db.ImportLegacyJSON(legacy); err != nil || imported {
		t.Fatalf("missing file: imported = %v, err = %v", imported, err)
	}

	snap := `{"totals":{"requests":9,"hits":8,"misses":1,"bytesSaved":900},
		"events":[{"time":"2024-01-01T10:00:00Z","result":"hit","module":"m"}]}`
	if err := os.WriteFile(legacy, []byte(snap), 0o644); err != nil {
		t.Fatal(err)
	}
	imported, err := db.ImportLegacyJSON(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if !imported {
		t.Fatal("legacy snapshot not imported")
	}
	if _, err := os.Stat(legacy + ".imported"); err != nil {
		t.Errorf("legacy file not renamed: %v", err)
	}

	totals, err := db.LoadTotals()
	if err != nil {
		t.Fatal(err)
	}
	if totals.Requests != 9 || totals.Hits != 8 || totals.BytesSaved != 900 {
		t.Errorf("imported totals = %+v", totals)
	}
	evs, err := db.RecentEvents(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Module != "m" {
		t.Errorf("imported events = %+v", evs)
	}

	// A used database never imports, even if a stray file appears.
	if err := os.WriteFile(legacy, []byte(snap), 0o644); err != nil {
		t.Fatal(err)
	}
	imported, err = db.ImportLegacyJSON(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if imported {
		t.Error("imported into a non-fresh database")
	}
}

// TestHistory_ImportLegacyJSONRetriesAfterFailure pins the atomicity: a
// failed import must leave the database fresh (nothing partially written),
// so the next start retries and imports the full legacy snapshot.
func TestHistory_ImportLegacyJSONRetriesAfterFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "stats.db")
	legacy := filepath.Join(dir, "stats.json")
	snap := `{"totals":{"requests":9,"hits":8,"bytesSaved":900},
		"events":[{"time":"2024-01-01T10:00:00Z","result":"hit","module":"m"}]}`
	if err := os.WriteFile(legacy, []byte(snap), 0o644); err != nil {
		t.Fatal(err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ImportLegacyJSON(legacy); err == nil {
		t.Fatal("import on a closed database should fail")
	}

	// The failed import left the database fresh: a retry imports fully.
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	imported, err := db.ImportLegacyJSON(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if !imported {
		t.Fatal("retry after a failed import did not import")
	}
	totals, err := db.LoadTotals()
	if err != nil {
		t.Fatal(err)
	}
	if totals.Requests != 9 || totals.Hits != 8 || totals.BytesSaved != 900 {
		t.Errorf("imported totals = %+v", totals)
	}
	evs, err := db.RecentEvents(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Module != "m" {
		t.Errorf("imported events = %+v", evs)
	}
}

// TestHistory_CollectorPersistsAcrossRestart is the end-to-end guarantee:
// counters, history buckets and the event feed survive a restart through
// the SQLite recorder.
func TestHistory_CollectorPersistsAcrossRestart(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "stats.db")

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s := stats.New(db, stats.WithTickInterval(5*time.Millisecond), stats.WithFlushInterval(10*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	// Wait for the first flush cycle, so Run has taken its delta baseline.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, to, err := db.Bounds(); err != nil {
			t.Fatal(err)
		} else if !to.IsZero() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.Hit("example.com/m", "v1.0.0", "zip", 100, time.Millisecond)
	s.Miss("example.com/m", "v1.1.0", "zip", 500, 500, time.Millisecond, true)

	for time.Now().Before(deadline) {
		if len(mustRecent(t, db, 10)) >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// "Reboot": open a fresh collector on the same database.
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	totals, err := db.LoadTotals()
	if err != nil {
		t.Fatal(err)
	}
	if totals.Hits != 1 || totals.Misses != 1 || totals.BytesSaved != 100 {
		t.Errorf("persisted totals = %+v, want 1 hit 1 miss 100 saved", totals)
	}
	pts, err := db.Series(time.Now().Add(-time.Minute), time.Now().Add(time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var bucketHits int64
	for _, p := range pts {
		bucketHits += p.Hits
	}
	if bucketHits != 1 {
		t.Errorf("persisted history buckets hold %d hits, want 1", bucketHits)
	}
	evs := mustRecent(t, db, 10)
	if len(evs) != 2 || evs[0].Result != "miss" {
		t.Errorf("persisted events = %+v, want newest-first miss then hit", evs)
	}
}

func mustRecent(t *testing.T, db *DB, n int) []stats.Event {
	t.Helper()
	evs, err := db.RecentEvents(n)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// TestHistory_MigratesLegacySchema opens a database created before the
// neighbour-cache counters existed: the missing columns are added in place
// and the accumulated statistics survive the upgrade.
func TestHistory_MigratesLegacySchema(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "stats.db")
	legacy, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE totals (
			id INTEGER PRIMARY KEY CHECK (id = 0),
			requests INTEGER NOT NULL DEFAULT 0,
			hits INTEGER NOT NULL DEFAULT 0,
			misses INTEGER NOT NULL DEFAULT 0,
			passes INTEGER NOT NULL DEFAULT 0,
			verifications INTEGER NOT NULL DEFAULT 0,
			verify_failures INTEGER NOT NULL DEFAULT 0,
			bytes_down INTEGER NOT NULL DEFAULT 0,
			bytes_up INTEGER NOT NULL DEFAULT 0,
			bytes_saved INTEGER NOT NULL DEFAULT 0
		)`,
		`INSERT INTO totals (id, requests, hits, bytes_saved) VALUES (0, 7, 5, 900)`,
		`CREATE TABLE buckets (
			bucket INTEGER PRIMARY KEY,
			requests INTEGER NOT NULL,
			hits INTEGER NOT NULL,
			misses INTEGER NOT NULL,
			passes INTEGER NOT NULL,
			verifications INTEGER NOT NULL,
			verify_failures INTEGER NOT NULL,
			bytes_down INTEGER NOT NULL,
			bytes_up INTEGER NOT NULL,
			bytes_saved INTEGER NOT NULL,
			store_bytes INTEGER NOT NULL,
			store_modules INTEGER NOT NULL,
			store_versions INTEGER NOT NULL,
			store_files INTEGER NOT NULL
		)`,
	} {
		if _, err := legacy.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := legacy.Exec(`INSERT INTO buckets (bucket, requests, hits, misses,
			passes, verifications, verify_failures, bytes_down, bytes_up,
			bytes_saved, store_bytes, store_modules, store_versions, store_files)
			VALUES (?, 3, 2, 1, 0, 0, 0, 30, 10, 20, 4096, 1, 1, 1)`,
		time.Now().UTC().Truncate(time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	got, err := db.LoadTotals()
	if err != nil {
		t.Fatal(err)
	}
	if got.Requests != 7 || got.Hits != 5 || got.BytesSaved != 900 {
		t.Errorf("legacy totals = %+v, want the pre-upgrade counters", got)
	}
	if got.NeighbourHits != 0 || got.NeighbourMisses != 0 || got.BytesNeighbour != 0 {
		t.Errorf("legacy totals neighbour = %+v, want zeros", got)
	}

	// The new columns accept writes after the in-place migration.
	want := got
	want.NeighbourHits, want.NeighbourMisses, want.BytesNeighbour = 4, 2, 600
	if err := db.SaveTotals(want); err != nil {
		t.Fatal(err)
	}
	if got, err = db.LoadTotals(); err != nil {
		t.Fatal(err)
	} else if got != want {
		t.Errorf("totals after upgrade = %+v, want %+v", got, want)
	}

	now := time.Now().UTC()
	pts, err := db.Series(now.Add(-time.Hour), now.Add(time.Hour), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 1 || pts[0].Requests != 3 || pts[0].BytesNeighbour != 0 {
		t.Errorf("series after upgrade = %+v", pts)
	}
}
