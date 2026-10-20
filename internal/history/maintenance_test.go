package history

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"paepcke.de/goproxy/internal/clients"
	"paepcke.de/goproxy/internal/stats"
)

func nightlyDB(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stats.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

func TestHistory_NextNightly(t *testing.T) {
	t.Parallel()
	now := time.Now()
	next := NextNightly(now)
	if !next.After(now) {
		t.Fatalf("next = %v, want after now %v", next, now)
	}
	if next.Hour() != MaintenanceHour || next.Minute() != 0 || next.Second() != 0 {
		t.Errorf("next = %v, want %02d:00:00 local", next, MaintenanceHour)
	}
	// Asking from the window itself rolls to the next day.
	if got := NextNightly(next); !got.Equal(next.AddDate(0, 0, 1)) {
		t.Errorf("next from window = %v, want %v", got, next.AddDate(0, 0, 1))
	}
}

func TestHistory_NightlyMaintenance(t *testing.T) {
	t.Parallel()
	db, path := nightlyDB(t)
	now := time.Date(2026, 10, 20, 23, 0, 0, 0, time.Local)

	if err := db.SaveTotals(stats.Totals{Requests: 5, Hits: 4, Misses: 1, BytesDown: 4096}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordBucket(stats.Bucket{
		Bucket: now.UTC().Truncate(time.Minute),
		Totals: stats.Totals{Requests: 5, Hits: 4, Misses: 1, BytesDown: 4096},
		Store:  stats.StoreStat{Bytes: 8192, Modules: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.AppendEvents([]stats.Event{{Time: now.UTC(), Result: "hit"}}); err != nil {
		t.Fatal(err)
	}

	if !db.MaintenanceDue(now) {
		t.Error("fresh database with no archive must be due")
	}
	rep, err := db.Nightly(context.Background(), now, "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Check.OK {
		t.Errorf("semantic check = %+v, want clean", rep.Check)
	}
	if rep.Backups != 1 {
		t.Errorf("generations = %d, want 1", rep.Backups)
	}
	if rep.BackupSize == 0 || rep.LiveBytes == 0 {
		t.Errorf("sizes = backup %d live %d, want both > 0", rep.BackupSize, rep.LiveBytes)
	}
	if !strings.HasSuffix(rep.Backup, ".db.gz") {
		t.Errorf("archive = %q, want a .db.gz", rep.Backup)
	}
	if _, err := os.Stat(rep.Backup); err != nil {
		t.Fatalf("archive missing: %v", err)
	}
	// The archive must decompress to a database that passes quick_check.
	verifyArchived(t, rep.Backup)
	if db.MaintenanceDue(now) {
		t.Error("after a window the database must not be due")
	}

	// The live database rests with an empty WAL after the window.
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() != 0 {
		t.Errorf("wal size = %d, want 0 after the final checkpoint", fi.Size())
	}
}

func TestHistory_NightlyCyclesEightGenerations(t *testing.T) {
	t.Parallel()
	db, _ := nightlyDB(t)
	base := time.Date(2026, 1, 1, 23, 0, 0, 0, time.Local)
	for i := 0; i < BackupGenerations+4; i++ {
		when := base.AddDate(0, 0, i)
		if _, err := db.Nightly(context.Background(), when, "v1.0.0"); err != nil {
			t.Fatalf("nightly %d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(db.backupDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != BackupGenerations {
		t.Errorf("archives = %d, want %d", len(entries), BackupGenerations)
	}
}

func TestHistory_SemanticCheckFlagsInconsistencies(t *testing.T) {
	t.Parallel()
	db, _ := nightlyDB(t)
	now := time.Now().UTC()

	if rep := db.semanticCheck(context.Background(), now); !rep.OK {
		t.Fatalf("fresh database = %+v, want clean", rep)
	}
	// Bucket history ahead of the lifetime totals: a real writer failure.
	if err := db.RecordBucket(stats.Bucket{
		Bucket: now.Truncate(time.Minute),
		Totals: stats.Totals{Requests: 3},
		Store:  stats.StoreStat{Bytes: 1024},
	}); err != nil {
		t.Fatal(err)
	}
	rep := db.semanticCheck(context.Background(), now)
	if rep.OK {
		t.Fatal("bucket sum ahead of totals must be flagged")
	}
	if !strings.Contains(strings.Join(rep.Errors, " "), "exceeds lifetime total") {
		t.Errorf("errors = %v, want the totals/bucket mismatch", rep.Errors)
	}
	if rep.Buckets != 1 || rep.BucketRequests != 3 {
		t.Errorf("counts = %+v, want 1 bucket with 3 requests", rep)
	}

	// Future-dated events are flagged as well.
	if err := db.AppendEvents([]stats.Event{
		{Time: now.Add(2 * time.Hour), Result: "hit"},
	}); err != nil {
		t.Fatal(err)
	}
	if rep := db.semanticCheck(context.Background(), now); rep.OK {
		t.Error("future-dated event must be flagged")
	}
}

func verifyArchived(t *testing.T, path string) {
	t.Helper()
	src, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	zr, err := gzip.NewReader(src)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	dst := filepath.Join(t.TempDir(), "restored.db")
	f, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(f, zr); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := verifySnapshot(dst); err != nil {
		t.Fatalf("archived database invalid: %v", err)
	}
}

func TestHistory_NightlyCleansCrashLeftovers(t *testing.T) {
	t.Parallel()
	db, _ := nightlyDB(t)
	dir := db.backupDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "stats-20200101T000000-v1.db.tmp")
	fresh := filepath.Join(dir, "stats-20200102T000000-v1.db.tmp")
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Nightly(context.Background(), time.Now(), "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale temp file still exists: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh temp file must survive: %v", err)
	}
}

func TestHistory_NightlyCapsStoredClients(t *testing.T) {
	t.Parallel()
	db, _ := nightlyDB(t)
	usage := make(map[string]clients.Usage, clients.MaxTracked+10)
	for i := range clients.MaxTracked + 10 {
		id := fmt.Sprintf("10.0.%d.%d", i/256, i%256)
		usage[id] = clients.Usage{Requests: int64(i + 1), LastSeen: time.Now().UTC()}
	}
	if err := db.SaveClients(usage); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Nightly(context.Background(), time.Now(), "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	got, err := db.LoadClients(clients.MaxTracked * 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > clients.MaxTracked {
		t.Errorf("stored clients = %d, want at most %d", len(got), clients.MaxTracked)
	}
	if _, ok := got["10.0.16.9"]; !ok {
		t.Error("the busiest client must survive the cap")
	}
}
