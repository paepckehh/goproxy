// Package history stores long-term proxy statistics in SQLite. The driver
// is pure Go (modernc.org/sqlite), so the single binary stays cgo-free.
//
// The database holds three tables: lifetime totals, one bucket per
// wall-clock minute of activity (with cache-store size snapshots), and the
// rolling event log. The stats collector writes in batches off the request
// path; the dashboard reads aggregated series. Everything survives
// reboots: lifetime counters, the full minute-resolution history (kept
// forever, about half a million rows per year), and the recent event feed
// (bounded by eventRetention).
package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"paepcke.de/goproxy/internal/clients"
	"paepcke.de/goproxy/internal/stats"
	"paepcke.de/goproxy/internal/store"
)

const (
	// eventRetention bounds the event log. Buckets and totals are kept
	// forever: they are the long-term history the dashboard charts.
	eventRetention = 90 * 24 * time.Hour
	pruneInterval  = 24 * time.Hour
	recentEvents   = 512
)

// DB is the long-term stats database. All methods are safe for concurrent
// use; a single connection serializes the tiny writes and the rare reads,
// so the recorder and the dashboard API never contend.
type DB struct {
	db            *sql.DB
	path          string
	pruneInterval time.Duration

	// mu serializes the nightly maintenance window (Lock) with every
	// recorder write and dashboard read (RLock): VACUUM and the snapshot
	// never race a flush, and no write is lost to a busy database.
	mu sync.RWMutex
}

// Open creates or opens the database at path with WAL journaling.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("history: %w", err)
	}
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(10000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("history: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &DB{db: db, path: path, pruneInterval: pruneInterval}, nil
}

func migrate(db *sql.DB) error {
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS totals (
			id INTEGER PRIMARY KEY CHECK (id = 0),
			requests INTEGER NOT NULL DEFAULT 0,
			hits INTEGER NOT NULL DEFAULT 0,
			misses INTEGER NOT NULL DEFAULT 0,
			passes INTEGER NOT NULL DEFAULT 0,
			verifications INTEGER NOT NULL DEFAULT 0,
			verify_failures INTEGER NOT NULL DEFAULT 0,
			bytes_down INTEGER NOT NULL DEFAULT 0,
			bytes_up INTEGER NOT NULL DEFAULT 0,
			bytes_saved INTEGER NOT NULL DEFAULT 0,
			bytes_neighbour INTEGER NOT NULL DEFAULT 0,
			neighbour_hits INTEGER NOT NULL DEFAULT 0,
			neighbour_misses INTEGER NOT NULL DEFAULT 0
		)`,
		`INSERT OR IGNORE INTO totals (id) VALUES (0)`,
		`CREATE TABLE IF NOT EXISTS buckets (
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
			bytes_neighbour INTEGER NOT NULL DEFAULT 0,
			neighbour_hits INTEGER NOT NULL DEFAULT 0,
			neighbour_misses INTEGER NOT NULL DEFAULT 0,
			store_bytes INTEGER NOT NULL,
			store_modules INTEGER NOT NULL,
			store_versions INTEGER NOT NULL,
			store_files INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			time INTEGER NOT NULL,
			result TEXT NOT NULL,
			module TEXT NOT NULL DEFAULT '',
			version TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL DEFAULT '',
			status INTEGER NOT NULL DEFAULT 0,
			bytes INTEGER NOT NULL DEFAULT 0,
			dur_ms INTEGER NOT NULL DEFAULT 0,
			detail TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS events_time ON events (time)`,
		`CREATE TABLE IF NOT EXISTS module_usage (
			path TEXT PRIMARY KEY,
			requests INTEGER NOT NULL DEFAULT 0,
			served_bytes INTEGER NOT NULL DEFAULT 0,
			last_used INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS clients (
			id TEXT PRIMARY KEY,
			requests INTEGER NOT NULL DEFAULT 0,
			bytes_down INTEGER NOT NULL DEFAULT 0,
			last_seen INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS clients_requests ON clients (requests DESC)`,
		`CREATE INDEX IF NOT EXISTS clients_bytes ON clients (bytes_down DESC)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("history: migrate: %w", err)
		}
	}
	// Databases created by older releases gain the neighbour-cache
	// counters in place; their existing rows default to zero.
	for _, table := range []string{"totals", "buckets"} {
		if err := ensureColumns(db, table, neighbourColumns); err != nil {
			return err
		}
	}
	return nil
}

// neighbourColumns track traffic served by neighbour caches. They live in
// both the lifetime totals and the minute buckets.
var neighbourColumns = []struct{ name, decl string }{
	{"bytes_neighbour", "INTEGER NOT NULL DEFAULT 0"},
	{"neighbour_hits", "INTEGER NOT NULL DEFAULT 0"},
	{"neighbour_misses", "INTEGER NOT NULL DEFAULT 0"},
}

// ensureColumns adds missing columns to an existing table, so a schema
// upgrade never requires dropping the accumulated statistics.
func ensureColumns(db *sql.DB, table string, cols []struct{ name, decl string }) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return fmt.Errorf("history: migrate: %w", err)
	}
	have := make(map[string]bool)
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, decl       string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &decl, &notNull, &dflt, &pk); err != nil {
			rows.Close()
			return fmt.Errorf("history: migrate: %w", err)
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("history: migrate: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("history: migrate: %w", err)
	}
	for _, c := range cols {
		if have[c.name] {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + c.name + ` ` + c.decl); err != nil {
			return fmt.Errorf("history: migrate: %w", err)
		}
	}
	return nil
}

// Close checkpoints the WAL and closes the database.
func (d *DB) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, _ = d.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`) // best effort
	return d.db.Close()
}

// Run prunes the event log periodically until ctx is done. Prune failures
// are best effort: the collector surfaces persistent recorder errors as
// persistErrors on the dashboard.
func (d *DB) Run(ctx context.Context) {
	d.prune(time.Now())
	t := time.NewTicker(d.pruneInterval)
	defer t.Stop()
	for {
		select {
		case now := <-t.C:
			d.prune(now)
		case <-ctx.Done():
			return
		}
	}
}

// prune deletes events older than the retention window.
func (d *DB) prune(now time.Time) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	_, _ = d.db.Exec(`DELETE FROM events WHERE time < ?`, now.Add(-eventRetention).Unix())
}

// SaveTotals upserts the lifetime counters (stats.Recorder).
func (d *DB) SaveTotals(t stats.Totals) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return saveTotalsTx(d.db, t)
}

// saveTotalsTx upserts the lifetime counters on an open transaction.
func saveTotalsTx(tx interface {
	Exec(query string, args ...any) (sql.Result, error)
}, t stats.Totals) error {
	_, err := tx.Exec(`UPDATE totals SET
			requests = ?, hits = ?, misses = ?, passes = ?,
			verifications = ?, verify_failures = ?,
			bytes_down = ?, bytes_up = ?, bytes_saved = ?,
			bytes_neighbour = ?, neighbour_hits = ?, neighbour_misses = ?
		WHERE id = 0`,
		t.Requests, t.Hits, t.Misses, t.Passes,
		t.Verifications, t.VerifyFailures,
		t.BytesDown, t.BytesUp, t.BytesSaved,
		t.BytesNeighbour, t.NeighbourHits, t.NeighbourMisses)
	if err != nil {
		return fmt.Errorf("history: totals: %w", err)
	}
	return nil
}

// LoadTotals returns the persisted lifetime counters.
func (d *DB) LoadTotals() (stats.Totals, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var t stats.Totals
	err := d.db.QueryRow(`SELECT requests, hits, misses, passes,
			verifications, verify_failures, bytes_down, bytes_up, bytes_saved,
			bytes_neighbour, neighbour_hits, neighbour_misses
		FROM totals WHERE id = 0`).
		Scan(&t.Requests, &t.Hits, &t.Misses, &t.Passes,
			&t.Verifications, &t.VerifyFailures,
			&t.BytesDown, &t.BytesUp, &t.BytesSaved,
			&t.BytesNeighbour, &t.NeighbourHits, &t.NeighbourMisses)
	if err != nil {
		return stats.Totals{}, fmt.Errorf("history: load totals: %w", err)
	}
	return t, nil
}

// SaveModuleUsage upserts per-module access counters. Absolute values are
// written, so a later restart restores the lifetime served bandwidth the
// dashboard's top-modules chart shows.
func (d *DB) SaveModuleUsage(usage map[string]store.ModuleUsage) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if len(usage) == 0 {
		return nil
	}
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("history: module usage: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO module_usage (path, requests, served_bytes, last_used)
		VALUES (?,?,?,?)
		ON CONFLICT (path) DO UPDATE SET
			requests = excluded.requests,
			served_bytes = excluded.served_bytes,
			last_used = excluded.last_used`)
	if err != nil {
		return fmt.Errorf("history: module usage: %w", err)
	}
	defer stmt.Close()
	for path, u := range usage {
		var lastUsed int64
		if !u.LastUsed.IsZero() {
			lastUsed = u.LastUsed.Unix()
		}
		if _, err := stmt.Exec(path, u.Requests, u.Served, lastUsed); err != nil {
			return fmt.Errorf("history: module usage: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("history: module usage: %w", err)
	}
	return nil
}

// LoadModuleUsage returns the persisted per-module access counters.
func (d *DB) LoadModuleUsage() (map[string]store.ModuleUsage, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	rows, err := d.db.Query(`SELECT path, requests, served_bytes, last_used FROM module_usage`)
	if err != nil {
		return nil, fmt.Errorf("history: module usage: %w", err)
	}
	defer rows.Close()
	usage := make(map[string]store.ModuleUsage)
	for rows.Next() {
		var path string
		var u store.ModuleUsage
		var lastUsed int64
		if err := rows.Scan(&path, &u.Requests, &u.Served, &lastUsed); err != nil {
			return nil, fmt.Errorf("history: module usage: %w", err)
		}
		if lastUsed != 0 {
			u.LastUsed = time.Unix(lastUsed, 0).UTC()
		}
		usage[path] = u
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history: module usage: %w", err)
	}
	return usage, nil
}

// SaveClients upserts per-client access counters. Absolute values are
// written, so a restart restores the long-term client ranking.
func (d *DB) SaveClients(usage map[string]clients.Usage) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if len(usage) == 0 {
		return nil
	}
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("history: clients: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO clients (id, requests, bytes_down, last_seen)
		VALUES (?,?,?,?)
		ON CONFLICT (id) DO UPDATE SET
			requests = excluded.requests,
			bytes_down = excluded.bytes_down,
			last_seen = excluded.last_seen`)
	if err != nil {
		return fmt.Errorf("history: clients: %w", err)
	}
	defer stmt.Close()
	for id, u := range usage {
		var lastSeen int64
		if !u.LastSeen.IsZero() {
			lastSeen = u.LastSeen.Unix()
		}
		if _, err := stmt.Exec(id, u.Requests, u.BytesDown, lastSeen); err != nil {
			return fmt.Errorf("history: clients: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("history: clients: %w", err)
	}
	return nil
}

// LoadClients returns the most active persisted clients, bounded so a long
// history of addresses never inflates the in-memory table at startup.
func (d *DB) LoadClients(limit int) (map[string]clients.Usage, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	rows, err := d.db.Query(`SELECT id, requests, bytes_down, last_seen
		FROM clients ORDER BY requests DESC, bytes_down DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("history: clients: %w", err)
	}
	defer rows.Close()
	usage := make(map[string]clients.Usage)
	for rows.Next() {
		var id string
		var u clients.Usage
		var lastSeen int64
		if err := rows.Scan(&id, &u.Requests, &u.BytesDown, &lastSeen); err != nil {
			return nil, fmt.Errorf("history: clients: %w", err)
		}
		if lastSeen != 0 {
			u.LastSeen = time.Unix(lastSeen, 0).UTC()
		}
		usage[id] = u
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history: clients: %w", err)
	}
	return usage, nil
}

// RecordBucket upserts one minute of activity (stats.Recorder): counter
// deltas merge when several flushes land in the same minute, store size
// snapshots overwrite (last sample wins).
func (d *DB) RecordBucket(b stats.Bucket) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	key := b.Bucket.Truncate(time.Minute).Unix()
	_, err := d.db.Exec(`INSERT INTO buckets (
			bucket, requests, hits, misses, passes,
			verifications, verify_failures,
			bytes_down, bytes_up, bytes_saved,
			bytes_neighbour, neighbour_hits, neighbour_misses,
			store_bytes, store_modules, store_versions, store_files
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (bucket) DO UPDATE SET
			requests = requests + excluded.requests,
			hits = hits + excluded.hits,
			misses = misses + excluded.misses,
			passes = passes + excluded.passes,
			verifications = verifications + excluded.verifications,
			verify_failures = verify_failures + excluded.verify_failures,
			bytes_down = bytes_down + excluded.bytes_down,
			bytes_up = bytes_up + excluded.bytes_up,
			bytes_saved = bytes_saved + excluded.bytes_saved,
			bytes_neighbour = bytes_neighbour + excluded.bytes_neighbour,
			neighbour_hits = neighbour_hits + excluded.neighbour_hits,
			neighbour_misses = neighbour_misses + excluded.neighbour_misses,
			store_bytes = excluded.store_bytes,
			store_modules = excluded.store_modules,
			store_versions = excluded.store_versions,
			store_files = excluded.store_files`,
		key, b.Requests, b.Hits, b.Misses, b.Passes,
		b.Verifications, b.VerifyFailures,
		b.BytesDown, b.BytesUp, b.BytesSaved,
		b.BytesNeighbour, b.NeighbourHits, b.NeighbourMisses,
		b.Store.Bytes, b.Store.Modules, b.Store.Versions, b.Store.Files)
	if err != nil {
		return fmt.Errorf("history: bucket: %w", err)
	}
	return nil
}

// AppendEvents inserts a batch of events in one transaction: either every
// event of the batch lands, or none does, so failed batches can be retried
// without duplicates.
func (d *DB) AppendEvents(evs []stats.Event) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if len(evs) == 0 {
		return nil
	}
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("history: events: %w", err)
	}
	defer tx.Rollback()
	if err := appendEventsTx(tx, evs); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("history: events: %w", err)
	}
	return nil
}

// appendEventsTx inserts a batch of events on an open transaction.
func appendEventsTx(tx *sql.Tx, evs []stats.Event) error {
	stmt, err := tx.Prepare(`INSERT INTO events (
			time, result, module, version, kind, status, bytes, dur_ms, detail
		) VALUES (?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("history: events: %w", err)
	}
	defer stmt.Close()
	for _, ev := range evs {
		t := ev.Time
		if t.IsZero() {
			t = time.Now().UTC()
		}
		if _, err := stmt.Exec(t.Unix(), ev.Result, ev.Module, ev.Version,
			ev.Kind, ev.Status, ev.Bytes, ev.DurMS, ev.Detail); err != nil {
			return fmt.Errorf("history: events: %w", err)
		}
	}
	return nil
}

// RecentEvents returns up to n events, newest first.
func (d *DB) RecentEvents(n int) ([]stats.Event, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if n <= 0 {
		return nil, nil
	}
	rows, err := d.db.Query(`SELECT time, module, version, kind, result,
			status, bytes, dur_ms, detail
		FROM events ORDER BY id DESC LIMIT ?`, n)
	if err != nil {
		return nil, fmt.Errorf("history: recent events: %w", err)
	}
	defer rows.Close()
	var out []stats.Event
	for rows.Next() {
		var ev stats.Event
		var sec int64
		if err := rows.Scan(&sec, &ev.Module, &ev.Version, &ev.Kind, &ev.Result,
			&ev.Status, &ev.Bytes, &ev.DurMS, &ev.Detail); err != nil {
			return nil, fmt.Errorf("history: recent events: %w", err)
		}
		ev.Time = time.Unix(sec, 0).UTC()
		out = append(out, ev)
	}
	return out, rows.Err()
}

// Point is one aggregated history slot for the dashboard.
type Point struct {
	Time            time.Time `json:"time"`
	Requests        int64     `json:"requests"`
	Hits            int64     `json:"hits"`
	Misses          int64     `json:"misses"`
	Passes          int64     `json:"passes"`
	Verifications   int64     `json:"verifications"`
	VerifyFailures  int64     `json:"verifyFailures"`
	BytesDown       int64     `json:"bytesDown"`
	BytesUp         int64     `json:"bytesUp"`
	BytesSaved      int64     `json:"bytesSaved"`
	BytesNeighbour  int64     `json:"bytesNeighbour"`
	NeighbourHits   int64     `json:"neighbourHits"`
	NeighbourMisses int64     `json:"neighbourMisses"`
	StoreBytes      int64     `json:"storeBytes"`
	StoreModules    int64     `json:"storeModules"`
}

// Series aggregates minute buckets between from (inclusive) and to
// (exclusive) into slots of res. res below one minute is clamped to one
// minute; empty slots are skipped, points are time-keyed and ordered.
func (d *DB) Series(from, to time.Time, res time.Duration) ([]Point, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	stride := int64(res.Seconds())
	if stride < 60 {
		stride = 60
	}
	rows, err := d.db.Query(`SELECT bucket/?*?,
			SUM(requests), SUM(hits), SUM(misses), SUM(passes),
			SUM(verifications), SUM(verify_failures),
			SUM(bytes_down), SUM(bytes_up), SUM(bytes_saved),
			SUM(bytes_neighbour), SUM(neighbour_hits), SUM(neighbour_misses),
			MAX(store_bytes), MAX(store_modules)
		FROM buckets
		WHERE bucket >= ? AND bucket < ?
		GROUP BY 1 ORDER BY 1`,
		stride, stride, from.Unix(), to.Unix())
	if err != nil {
		return nil, fmt.Errorf("history: series: %w", err)
	}
	defer rows.Close()
	var out []Point
	for rows.Next() {
		var slot int64
		p := Point{}
		if err := rows.Scan(&slot, &p.Requests, &p.Hits, &p.Misses, &p.Passes,
			&p.Verifications, &p.VerifyFailures,
			&p.BytesDown, &p.BytesUp, &p.BytesSaved,
			&p.BytesNeighbour, &p.NeighbourHits, &p.NeighbourMisses,
			&p.StoreBytes, &p.StoreModules); err != nil {
			return nil, fmt.Errorf("history: series: %w", err)
		}
		p.Time = time.Unix(slot, 0).UTC()
		out = append(out, p)
	}
	return out, rows.Err()
}

// Summary aggregates one time range for the dashboard.
type Summary struct {
	Requests        int64 `json:"requests"`
	Hits            int64 `json:"hits"`
	Misses          int64 `json:"misses"`
	Passes          int64 `json:"passes"`
	Verifications   int64 `json:"verifications"`
	VerifyFailures  int64 `json:"verifyFailures"`
	BytesDown       int64 `json:"bytesDown"`
	BytesUp         int64 `json:"bytesUp"`
	BytesSaved      int64 `json:"bytesSaved"`
	BytesNeighbour  int64 `json:"bytesNeighbour"`
	NeighbourHits   int64 `json:"neighbourHits"`
	NeighbourMisses int64 `json:"neighbourMisses"`
	StoreStart      int64 `json:"storeStart"` // first observed store size
	StoreEnd        int64 `json:"storeEnd"`   // last observed store size
}

// RangeSummary aggregates the buckets between from (inclusive) and to
// (exclusive), plus the first and last observed cache-store sizes.
func (d *DB) RangeSummary(from, to time.Time) (Summary, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var s Summary
	var first, last sql.NullInt64
	err := d.db.QueryRow(`SELECT
			COALESCE(SUM(requests), 0), COALESCE(SUM(hits), 0),
			COALESCE(SUM(misses), 0), COALESCE(SUM(passes), 0),
			COALESCE(SUM(verifications), 0), COALESCE(SUM(verify_failures), 0),
			COALESCE(SUM(bytes_down), 0), COALESCE(SUM(bytes_up), 0),
			COALESCE(SUM(bytes_saved), 0),
			COALESCE(SUM(bytes_neighbour), 0),
			COALESCE(SUM(neighbour_hits), 0),
			COALESCE(SUM(neighbour_misses), 0),
			(SELECT store_bytes FROM buckets
			 WHERE bucket >= ? AND bucket < ?
			 ORDER BY bucket LIMIT 1),
			(SELECT store_bytes FROM buckets
			 WHERE bucket >= ? AND bucket < ?
			 ORDER BY bucket DESC LIMIT 1)
		FROM buckets WHERE bucket >= ? AND bucket < ?`,
		from.Unix(), to.Unix(), from.Unix(), to.Unix(), from.Unix(), to.Unix()).
		Scan(&s.Requests, &s.Hits, &s.Misses, &s.Passes,
			&s.Verifications, &s.VerifyFailures,
			&s.BytesDown, &s.BytesUp, &s.BytesSaved,
			&s.BytesNeighbour, &s.NeighbourHits, &s.NeighbourMisses,
			&first, &last)
	if err != nil {
		return Summary{}, fmt.Errorf("history: range summary: %w", err)
	}
	s.StoreStart, s.StoreEnd = first.Int64, last.Int64
	return s, nil
}

// Bounds returns the earliest and latest bucket times. Both are the zero
// time when no buckets exist yet.
func (d *DB) Bounds() (from, to time.Time, err error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var minS, maxS sql.NullInt64
	err = d.db.QueryRow(`SELECT MIN(bucket), MAX(bucket) FROM buckets`).
		Scan(&minS, &maxS)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("history: bounds: %w", err)
	}
	if minS.Valid {
		from = time.Unix(minS.Int64, 0).UTC()
	}
	if maxS.Valid {
		to = time.Unix(maxS.Int64, 0).UTC()
	}
	return from, to, nil
}

// ImportLegacyJSON migrates a pre-SQLite stats.json snapshot (lifetime
// totals plus the recent event ring) into a fresh database, then renames
// the file to stats.json.imported so the import runs exactly once. Returns
// whether an import happened.
func (d *DB) ImportLegacyJSON(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("history: legacy import: %w", err)
	}
	var snap struct {
		Totals stats.Totals
		Events []stats.Event
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		return false, fmt.Errorf("history: legacy import: %w", err)
	}
	t, err := d.LoadTotals()
	if err != nil {
		return false, err
	}
	var n int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		return false, fmt.Errorf("history: legacy import: %w", err)
	}
	if t != (stats.Totals{}) || n > 0 {
		return false, nil // not a fresh database
	}
	// One transaction: a failed import leaves the database fresh, so the
	// next start retries it instead of silently dropping the legacy data.
	tx, err := d.db.Begin()
	if err != nil {
		return false, fmt.Errorf("history: legacy import: %w", err)
	}
	defer tx.Rollback()
	if err := appendEventsTx(tx, snap.Events); err != nil {
		return false, err
	}
	if err := saveTotalsTx(tx, snap.Totals); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("history: legacy import: %w", err)
	}
	if err := os.Rename(path, path+".imported"); err != nil {
		return false, fmt.Errorf("history: legacy import: %w", err)
	}
	return true, nil
}
