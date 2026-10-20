package history

import (
	"compress/gzip"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	// MaintenanceHour is the local wall-clock hour of the nightly window.
	// The proxy serves builds around the clock, so maintenance runs at
	// night and never reloads or restarts the process.
	MaintenanceHour = 23

	// BackupGenerations is how many daily archives are kept. The two most
	// recent are always retained, even when a prune runs early.
	BackupGenerations = 8

	// backupDirSuffix names the snapshot directory beside the live database.
	backupDirSuffix = ".backup.daily"

	// clientRetention drops client rows that saw no traffic for this long,
	// so the address table does not grow forever on a busy proxy.
	clientRetention = 90 * 24 * time.Hour

	// missedWindow is how old the newest archive may be before a starting
	// proxy considers the last nightly window missed and runs it. A daily
	// 23:00 run leaves the newest archive under 24 hours old; the slack
	// covers a slow window and a start just before the next one.
	missedWindow = 25 * time.Hour
)

// NextNightly returns the next local 23:00 at or after now. Calendar-day
// arithmetic keeps the wall-clock hour across DST transitions.
func NextNightly(now time.Time) time.Time {
	local := now.In(time.Local)
	fire := time.Date(local.Year(), local.Month(), local.Day(), MaintenanceHour, 0, 0, 0, time.Local)
	if !fire.After(now) {
		fire = fire.AddDate(0, 0, 1)
	}
	return fire
}

// CheckReport is the result of the nightly semantic database check. It
// never repairs anything: it counts and names every inconsistency so the
// operator sees the truth before the database is optimized and archived.
type CheckReport struct {
	OK     bool     `json:"ok"`
	Errors []string `json:"errors,omitempty"`

	Buckets        int64 `json:"buckets"`
	Events         int64 `json:"events"`
	Modules        int64 `json:"modules"`
	Clients        int64 `json:"clients"`
	TotalsRequests int64 `json:"totalsRequests"`
	BucketRequests int64 `json:"bucketRequests"`
}

// Report describes one completed maintenance window.
type Report struct {
	Started    time.Time   `json:"startedAt"`
	DurationMS int64       `json:"durationMs"`
	Check      CheckReport `json:"check"`
	Pruned     int64       `json:"clientsPruned"`
	LiveBytes  int64       `json:"liveBytes"`
	Backup     string      `json:"backup"`
	BackupSize int64       `json:"backupBytes"`
	Backups    int         `json:"backups"`
}

// Summary renders the window as one log/event line.
func (r Report) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "nightly stats maintenance: %dms, live db %s", r.DurationMS, humanBytes(r.LiveBytes))
	if r.Backup != "" {
		fmt.Fprintf(&b, ", backup %s (%s, %d kept)", filepath.Base(r.Backup), humanBytes(r.BackupSize), r.Backups)
	}
	if r.Pruned > 0 {
		fmt.Fprintf(&b, ", %d stale clients pruned", r.Pruned)
	}
	if r.Check.OK {
		b.WriteString(", semantic check ok")
	} else {
		fmt.Fprintf(&b, ", semantic check found %d problem(s): %s",
			len(r.Check.Errors), strings.Join(r.Check.Errors, "; "))
	}
	return b.String()
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// MaintenanceDue reports whether the last nightly window was missed: no
// archive exists yet, or the newest one is older than missedWindow. A proxy
// that was down at 23:00 catches up on its next start.
func (d *DB) MaintenanceDue(now time.Time) bool {
	newest, ok := newestBackup(d.backupDir())
	return !ok || now.Sub(newest) > missedWindow
}

// Nightly runs the full maintenance window: semantic check, WAL sync,
// database reorganization, client retention, a verified compressed
// snapshot with eight-generation cycling, and a final WAL checkpoint.
//
// The live database stays open for serving; "closed" here means the file
// rests complete with an empty WAL (wal_checkpoint TRUNCATE), which is the
// state a backup wants to find. The proxy's single SQLite connection
// serializes the maintenance statements with the recorder flushes.
func (d *DB) Nightly(ctx context.Context, now time.Time, version string) (Report, error) {
	start := time.Now()
	rep := Report{Started: start.UTC()}

	rep.Check = d.semanticCheck(ctx, now)
	// A failed check is information, not an abort: the archive is still
	// valuable and the next nightly re-checks the database. Tampered or
	// structurally broken files fail the backup verify instead.
	d.prune(now)

	// The exclusive lock keeps the recorder flush loop and the dashboard
	// readers out for the destructive part of the window. They block for
	// its duration instead of racing VACUUM or losing a write to a busy
	// database; the request hot path never waits on the database.
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, err := d.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return rep, fmt.Errorf("history: nightly checkpoint: %w", err)
	}
	// VACUUM rewrites the file without free pages and rebuilds every index:
	// the reorg step. ANALYZE refreshes the planner statistics afterwards.
	if _, err := d.db.ExecContext(ctx, `VACUUM`); err != nil {
		return rep, fmt.Errorf("history: nightly vacuum: %w", err)
	}
	if _, err := d.db.ExecContext(ctx, `ANALYZE`); err != nil {
		return rep, fmt.Errorf("history: nightly analyze: %w", err)
	}
	if _, err := d.db.ExecContext(ctx, `PRAGMA optimize`); err != nil {
		return rep, fmt.Errorf("history: nightly optimize: %w", err)
	}
	res, err := d.db.ExecContext(ctx, `DELETE FROM clients WHERE last_seen < ? AND last_seen > 0`,
		now.Add(-clientRetention).Unix())
	if err != nil {
		return rep, fmt.Errorf("history: nightly prune clients: %w", err)
	}
	rep.Pruned, _ = res.RowsAffected()

	archive, size, kept, err := d.backup(ctx, now, version)
	if err != nil {
		return rep, fmt.Errorf("history: nightly backup: %w", err)
	}
	rep.Backup, rep.BackupSize, rep.Backups = archive, size, kept
	if fi, err := os.Stat(d.path); err == nil {
		rep.LiveBytes = fi.Size()
	}

	// Every maintenance write is on disk, the WAL empty: the next process
	// (and any operator copying the file) finds a complete database.
	if _, err := d.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return rep, fmt.Errorf("history: nightly final checkpoint: %w", err)
	}
	rep.DurationMS = time.Since(start).Milliseconds()
	return rep, nil
}

// semanticCheck verifies the invariants no CHECK constraint can guarantee:
// SQLite page integrity, a present totals row, non-negative counters, and
// the lifetime request total never falling behind the bucket history.
func (d *DB) semanticCheck(ctx context.Context, now time.Time) CheckReport {
	d.mu.RLock()
	defer d.mu.RUnlock()
	rep := CheckReport{OK: true}
	fail := func(format string, args ...any) {
		rep.OK = false
		rep.Errors = append(rep.Errors, fmt.Sprintf(format, args...))
	}
	count := func(query string, args ...any) int64 {
		var n int64
		if err := d.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			fail("query %q: %v", query, err)
			return 0
		}
		return n
	}

	// SQLite integrity first: rows stay untouched until the scan is done
	// (the single connection cannot serve another statement mid-rows).
	rows, err := d.db.QueryContext(ctx, `PRAGMA quick_check`)
	if err != nil {
		fail("quick_check: %v", err)
	} else {
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				fail("quick_check: %v", err)
				break
			}
			if line != "ok" {
				fail("sqlite: %s", line)
			}
		}
		if err := rows.Err(); err != nil {
			fail("quick_check: %v", err)
		}
		rows.Close()
	}

	if count(`SELECT COUNT(*) FROM totals WHERE id = 0`) != 1 {
		fail("lifetime totals row missing")
	}
	if n := count(`SELECT COUNT(*) FROM totals WHERE requests < 0 OR hits < 0 OR misses < 0 OR
		passes < 0 OR verifications < 0 OR verify_failures < 0 OR bytes_down < 0 OR bytes_up < 0 OR
		bytes_saved < 0 OR bytes_neighbour < 0 OR neighbour_hits < 0 OR neighbour_misses < 0`); n > 0 {
		fail("%d lifetime row(s) with negative counters", n)
	}
	if n := count(`SELECT COUNT(*) FROM buckets WHERE requests < 0 OR hits < 0 OR misses < 0 OR
		passes < 0 OR verifications < 0 OR verify_failures < 0 OR bytes_down < 0 OR bytes_up < 0 OR
		bytes_saved < 0 OR bytes_neighbour < 0 OR neighbour_hits < 0 OR neighbour_misses < 0 OR
		store_bytes < 0 OR store_modules < 0 OR store_versions < 0 OR store_files < 0`); n > 0 {
		fail("%d bucket(s) with negative counters", n)
	}
	if n := count(`SELECT COUNT(*) FROM module_usage WHERE requests < 0 OR served_bytes < 0 OR last_used < 0`); n > 0 {
		fail("%d module row(s) with negative counters", n)
	}
	if n := count(`SELECT COUNT(*) FROM clients WHERE requests < 0 OR bytes_down < 0 OR last_seen < 0`); n > 0 {
		fail("%d client row(s) with negative counters", n)
	}
	if n := count(`SELECT COUNT(*) FROM events WHERE time > ?`, now.Add(time.Hour).Unix()); n > 0 {
		fail("%d event(s) dated more than an hour in the future", n)
	}
	if n := count(`SELECT COUNT(*) FROM clients WHERE last_seen > ?`, now.Add(time.Hour).Unix()); n > 0 {
		fail("%d client row(s) last seen more than an hour in the future", n)
	}

	rep.Buckets = count(`SELECT COUNT(*) FROM buckets`)
	rep.Events = count(`SELECT COUNT(*) FROM events`)
	rep.Modules = count(`SELECT COUNT(*) FROM module_usage`)
	rep.Clients = count(`SELECT COUNT(*) FROM clients`)
	rep.TotalsRequests = count(`SELECT COALESCE(requests, 0) FROM totals WHERE id = 0`)
	rep.BucketRequests = count(`SELECT COALESCE(SUM(requests), 0) FROM buckets`)
	if rep.BucketRequests > rep.TotalsRequests {
		fail("bucket request sum %d exceeds lifetime total %d", rep.BucketRequests, rep.TotalsRequests)
	}
	return rep
}

// backupDir is the snapshot directory beside the live database file.
func (d *DB) backupDir() string {
	return d.path + backupDirSuffix
}

// backup writes, verifies, compresses and cycles one snapshot. It returns
// the archive path, its size and how many archives were kept.
func (d *DB) backup(ctx context.Context, now time.Time, version string) (string, int64, int, error) {
	dir := d.backupDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, 0, err
	}
	stamp := now.In(time.Local).Format("20060102T150405")
	version = sanitizeVersion(version)
	snapshot := filepath.Join(dir, fmt.Sprintf("stats-%s-%s.db", stamp, version))
	// Build the snapshot under a temporary name and rename it into place
	// only after it verified clean: a crash mid-copy can never leave a
	// truncated file that looks like a rollback target or hides the next
	// missed window from MaintenanceDue.
	tmp := snapshot + ".tmp"
	if err := removeIfExists(tmp); err != nil {
		return "", 0, 0, err
	}
	if err := removeIfExists(snapshot); err != nil {
		return "", 0, 0, err
	}
	// VACUUM INTO takes a consistent point-in-time copy without blocking
	// writers for the whole run and writes a standalone file with no WAL.
	quoted := strings.ReplaceAll(tmp, `"`, `""`)
	if _, err := d.db.ExecContext(ctx, fmt.Sprintf(`VACUUM INTO "%s"`, quoted)); err != nil {
		return "", 0, 0, fmt.Errorf("vacuum into %s: %w", tmp, err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return "", 0, 0, err
	}
	if err := verifySnapshot(tmp); err != nil {
		return "", 0, 0, err
	}
	if err := os.Rename(tmp, snapshot); err != nil {
		return "", 0, 0, err
	}
	archive := snapshot
	if err := compressGzip(snapshot); err != nil {
		// Keep the uncompressed snapshot: the backup exists, just larger.
		archive = snapshot
	} else {
		archive = snapshot + ".gz"
	}
	fi, err := os.Stat(archive)
	if err != nil {
		return "", 0, 0, err
	}
	kept, err := pruneBackups(dir, BackupGenerations)
	if err != nil {
		return archive, fi.Size(), kept, err
	}
	return archive, fi.Size(), kept, nil
}

// verifySnapshot opens the freshly written snapshot read-only and
// immutable (no WAL/SHM sidecars) and requires quick_check to answer "ok".
func verifySnapshot(path string) error {
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro&immutable=1", url.PathEscape(path)))
	if err != nil {
		return fmt.Errorf("history: verify %s: %w", path, err)
	}
	defer db.Close()
	var res string
	if err := db.QueryRow(`PRAGMA quick_check`).Scan(&res); err != nil {
		return fmt.Errorf("history: verify %s: %w", path, err)
	}
	if res != "ok" {
		return fmt.Errorf("history: verify %s: %q", path, res)
	}
	return nil
}

// compressGzip writes path+".gz" and removes the original once the archive
// is complete and renamed into place.
func compressGzip(path string) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp := path + ".gz.tmp"
	dst, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	zw, err := gzip.NewWriterLevel(dst, gzip.DefaultCompression)
	if err != nil {
		dst.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := io.Copy(zw, src); err != nil {
		zw.Close()
		dst.Close()
		os.Remove(tmp)
		return err
	}
	if err := zw.Close(); err != nil {
		dst.Close()
		os.Remove(tmp)
		return err
	}
	if err := dst.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path+".gz"); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(path+".gz", 0o600); err != nil {
		return err
	}
	return os.Remove(path)
}

// pruneBackups keeps the newest keep archives (both .gz and any leftover
// uncompressed .db), always retaining the two most recent.
func pruneBackups(dir string, keep int) (int, error) {
	type file struct {
		path  string
		mtime time.Time
	}
	var files []file
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		name := e.Name()
		// Leftovers of a crashed copy or compression are never archives;
		// drop them once they are a day old instead of keeping them.
		if strings.HasSuffix(name, ".tmp") {
			if time.Since(info.ModTime()) > 24*time.Hour {
				_ = os.Remove(filepath.Join(dir, name))
			}
			continue
		}
		switch filepath.Ext(name) {
		case ".gz", ".db":
		default:
			continue
		}
		files = append(files, file{path: filepath.Join(dir, name), mtime: info.ModTime()})
	}
	slices.SortFunc(files, func(a, b file) int { return b.mtime.Compare(a.mtime) })
	for i, f := range files {
		if i < 2 || i < keep {
			continue
		}
		if err := os.Remove(f.path); err != nil {
			return 0, err
		}
	}
	if keep > len(files) {
		return len(files), nil
	}
	return keep, nil
}

// newestBackup returns the modification time of the newest archive.
func newestBackup(dir string) (time.Time, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return time.Time{}, false
	}
	var newest time.Time
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch filepath.Ext(e.Name()) {
		case ".gz", ".db":
		default:
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest, !newest.IsZero()
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// sanitizeVersion folds anything path-hostile in the version into '-'.
func sanitizeVersion(v string) string {
	if v == "" {
		return "dev"
	}
	return strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ' ', '\t', '\n', '\r':
			return '-'
		}
		return r
	}, v)
}
