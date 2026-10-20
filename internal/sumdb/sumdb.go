// Package sumdb verifies module content against a Go checksum database
// (sum.golang.org). It uses the transparency-log client from
// golang.org/x/mod/sumdb, so signed note signatures, tree consistency and
// tile-based inclusion proofs are all checked before a record is trusted.
//
// State (latest signed tree, lookup and tile caches) persists under a state
// directory so verification survives restarts.
package sumdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/module"
	"golang.org/x/mod/sumdb"
	"golang.org/x/mod/sumdb/dirhash"
	"golang.org/x/mod/sumdb/note"
	"golang.org/x/mod/sumdb/tlog"
)

// DefaultKey is the public key of sum.golang.org.
const DefaultKey = "sum.golang.org+033de0ae+Ac4zctda0e5eza+HJyk9SxEdh+s3Ux18htTTAD8OuAn8"

// ErrUnknown reports a module version not present in the checksum database.
var ErrUnknown = errors.New("sumdb: module not in checksum database")

// notFoundMarker is embedded only in ReadRemote errors for /lookup 404s.
// It is matched as text in Lookup because the x/mod client wraps errors
// with %v, breaking errors.Is. The marker contains spaces, so neither a
// module path nor a tile path (both space-free) can forge it: a 404 on a
// tile fetch is a database outage and must fail closed, not read as
// "module unknown".
const notFoundMarker = "module not in checksum database"

// ErrSecurity is returned when the checksum database misbehaves
// (bad signature, inconsistent tree, failed proof).
var ErrSecurity = sumdb.ErrSecurity

// Record is the verified checksum record of a module version.
type Record struct {
	ModHash string // h1: hash of the go.mod file
	ZipHash string // h1: hash of the module zip
	Raw     string // signed database note, kept as provenance
}

// DB is a checksum database client with persisted verification state.
type DB struct {
	base     *url.URL
	key      string
	name     string
	verifier note.Verifier
	dir      string
	http     *http.Client
	log      *slog.Logger

	// clientMu guards client. The x/mod client caches every lookup result,
	// including transient network and tile errors, for the lifetime of the
	// client. A long-running proxy must throw such a client away after a
	// failed lookup or one temporary outage would poison that module
	// version until the process restarts.
	clientMu sync.RWMutex
	client   *sumdb.Client
}

// New creates a database client for base (e.g. https://sum.golang.org)
// verified with the given public key. State persists under stateDir.
func New(base, key, stateDir string, client *http.Client, log *slog.Logger) (*DB, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("sumdb: invalid base url %q", base)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	verifier, err := note.NewVerifier(key)
	if err != nil {
		return nil, fmt.Errorf("sumdb: %w", err)
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if log == nil {
		log = slog.Default()
	}
	for _, d := range []string{"config", "cache"} {
		if err := os.MkdirAll(filepath.Join(stateDir, d), 0o755); err != nil {
			return nil, fmt.Errorf("sumdb: create state dir: %w", err)
		}
	}
	db := &DB{base: u, key: key, name: verifier.Name(), verifier: verifier,
		dir: stateDir, http: client, log: log}
	db.client = sumdb.NewClient(db)
	return db, nil
}

// clientSnapshot returns the current x/mod client. Callers must not hold the
// read lock while using it.
func (db *DB) clientSnapshot() *sumdb.Client {
	db.clientMu.RLock()
	defer db.clientMu.RUnlock()
	return db.client
}

// discardClient replaces the client after a failed lookup. The in-memory
// caches of the old client are dropped; persisted state under dir survives.
// Only the goroutine that still sees the same client replaces it, so
// concurrent failures do not churn through clients unnecessarily.
func (db *DB) discardClient(old *sumdb.Client) {
	db.clientMu.Lock()
	defer db.clientMu.Unlock()
	if db.client == old {
		db.client = sumdb.NewClient(db)
	}
}

// Name returns the checksum database name (e.g. "sum.golang.org"), the
// prefix go clients use on the proxy's /sumdb/ endpoint.
func (db *DB) Name() string { return db.name }

// Lookup returns the verified checksum record for module@version.
// Unknown module versions yield ErrUnknown.
func (db *DB) Lookup(mod, ver string) (*Record, error) {
	// The client filters a record's lines by "path version " prefix,
	// so the zip hash and the go.mod hash need one lookup each.
	// Both hit the same cached record, so the second is served from cache.
	client := db.clientSnapshot()
	zipLines, err := client.Lookup(mod, ver)
	if err != nil {
		// The x/mod client wraps errors with %v, breaking errors.Is;
		// the marker is only produced by our own ReadRemote for lookup
		// 404s. Any other error (tile outage, bad signature, ...) fails
		// closed via the caller. Do not reset on ErrUnknown: private
		// modules are expected to stay unverified without refetching
		// their negative lookup on every request.
		if strings.Contains(err.Error(), notFoundMarker) {
			return nil, ErrUnknown
		}
		db.discardClient(client)
		return nil, err
	}
	modLines, err := client.Lookup(mod, ver+"/go.mod")
	if err != nil {
		db.discardClient(client)
		return nil, err
	}
	lines := zipLines
	if len(modLines) > 0 {
		lines = append(lines, modLines[0])
	}
	rec := &Record{Raw: strings.Join(lines, "\n") + "\n"}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		hash := strings.TrimPrefix(fields[2], "h1:")
		switch fields[1] {
		case ver:
			rec.ZipHash = hash
		case ver + "/go.mod":
			rec.ModHash = hash
		}
	}
	if raw, err := db.ReadCache(db.lookupCacheFile(mod, ver)); err == nil {
		rec.Raw = string(raw)
	}
	return rec, nil
}

// Refresh fetches the latest signed tree head from the database server,
// verifies its signature, and persists it when it is newer than the head
// already stored. A long-running proxy therefore keeps a current view of the
// transparency log instead of only learning about new entries while it
// happens to verify a module. The x/mod client keeps the head in memory, so
// the client is replaced after the persisted head advances.
//
// ponytail: the signature is verified and the tree never shrinks, but the
// new head is not proven consistent with the previous root. Add a
// tlog.CheckTree consistency proof if a split-view log is in the threat
// model.
func (db *DB) Refresh(ctx context.Context) error {
	u := *db.base
	u.Path = db.base.Path + "/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("sumdb: latest tree head: %w", err)
	}
	resp, err := db.http.Do(req)
	if err != nil {
		return fmt.Errorf("sumdb: latest tree head: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sumdb: latest tree head: status %d", resp.StatusCode)
	}
	msg, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("sumdb: latest tree head: %w", err)
	}
	verifiers := note.VerifierList(db.verifier)
	verified, err := note.Open(msg, verifiers)
	if err != nil {
		return fmt.Errorf("sumdb: latest tree head: %w", err)
	}
	tree, err := tlog.ParseTree([]byte(verified.Text))
	if err != nil {
		return fmt.Errorf("sumdb: latest tree head: %w", err)
	}

	file := db.name + "/latest"
	advanced := false
	for range 3 {
		old, err := db.ReadConfig(file)
		if err != nil {
			return err
		}
		if treeSize(old, verifiers) >= tree.N {
			// Never roll the persisted head back; the stored one is at
			// least as new.
			return nil
		}
		err = db.WriteConfig(file, old, msg)
		if err == nil {
			advanced = true
			break
		}
		if !errors.Is(err, sumdb.ErrWriteConflict) {
			return fmt.Errorf("sumdb: persisting tree head: %w", err)
		}
	}
	if !advanced {
		// A concurrent writer kept winning; the next refresh retries.
		return nil
	}
	db.discardClient(db.clientSnapshot())
	db.log.Debug("sumdb: tree head refreshed", "tree", tree.N)
	return nil
}

// RefreshLoop refreshes the checksum database state at startup and then
// every interval until ctx is cancelled.
func (db *DB) RefreshLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if err := db.Refresh(ctx); err != nil && ctx.Err() == nil {
			db.log.Debug("sumdb: refreshing tree head", "err", err)
		}
		timer.Reset(interval)
	}
}

// treeSize returns the tree size of a signed note, or -1 when the note is
// absent or unparsable. An empty note is the valid empty timeline.
func treeSize(msg []byte, verifiers note.Verifiers) int64 {
	if len(msg) == 0 {
		return 0
	}
	n, err := note.Open(msg, verifiers)
	if err != nil {
		return -1
	}
	tree, err := tlog.ParseTree([]byte(n.Text))
	if err != nil {
		return -1
	}
	return tree.N
}

// lookupCacheFile mirrors the client's cache path for a lookup response.
func (db *DB) lookupCacheFile(mod, ver string) string {
	esc, _ := module.EscapePath(mod)
	escv, _ := module.EscapeVersion(ver)
	return db.name + "/lookup/" + esc + "@" + escv
}

// HashZip computes the h1: hash of a module zip file.
func HashZip(path string) (string, error) {
	return dirhash.HashZip(path, dirhash.DefaultHash)
}

// HashMod computes the h1: hash of a go.mod file's bytes.
func HashMod(data []byte) (string, error) {
	return dirhash.Hash1([]string{"go.mod"}, func(string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	})
}

// Base returns a copy of the database base URL, for reverse-proxying
// /sumdb/ requests to the same server.
func (db *DB) Base() *url.URL {
	u := *db.base
	return &u
}

// ReadRemote fetches a path (lookup or tile) from the database server.
func (db *DB) ReadRemote(path string) ([]byte, error) {
	u := *db.base
	u.Path = db.base.Path + path
	resp, err := db.http.Get(u.String())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		if strings.HasPrefix(path, "/lookup/") {
			return nil, fmt.Errorf("sumdb: %s: %s", path, notFoundMarker)
		}
		return nil, fmt.Errorf("sumdb: %s: status %d", path, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("sumdb: %s: status %d", path, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// ReadConfig returns a persisted config file ("key" or "<name>/latest").
// A missing latest file starts the client from an empty signed tree.
func (db *DB) ReadConfig(file string) ([]byte, error) {
	if file == "key" {
		return []byte(db.key), nil
	}
	data, err := os.ReadFile(filepath.Join(db.dir, "config", filepath.FromSlash(file)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return data, err
}

// WriteConfig atomically updates a config file from old to new.
func (db *DB) WriteConfig(file string, old, new []byte) error {
	path := filepath.Join(db.dir, "config", filepath.FromSlash(file))
	cur, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if len(old) != 0 {
			return sumdb.ErrWriteConflict
		}
	case err != nil:
		return err
	case !bytes.Equal(cur, old):
		return sumdb.ErrWriteConflict
	}
	if err := writeFileAtomic(path, new); err != nil {
		return err
	}
	// Only configuration (the latest signed tree head) needs the extra
	// directory sync; cache writes are validated after every read and are
	// re-fetched when lost, so they stay cheap.
	fsyncDir(filepath.Dir(path))
	return nil
}

// ReadCache returns a cached lookup or tile, or an error if absent.
func (db *DB) ReadCache(file string) ([]byte, error) {
	return os.ReadFile(filepath.Join(db.dir, "cache", filepath.FromSlash(file)))
}

// WriteCache atomically stores a lookup or tile.
func (db *DB) WriteCache(file string, data []byte) {
	path := filepath.Join(db.dir, "cache", filepath.FromSlash(file))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		db.log.Warn("sumdb: cache mkdir", "err", err)
		return
	}
	if err := writeFileAtomic(path, data); err != nil {
		db.log.Warn("sumdb: cache write", "file", file, "err", err)
	}
}

func (db *DB) Log(msg string) {
	db.log.Info("sumdb: " + msg)
}

// SecurityError reports database misbehavior; the client then fails closed.
func (db *DB) SecurityError(msg string) {
	db.log.Error("sumdb: SECURITY: " + msg)
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// Sync before the rename below: a torn cache file would poison lookups.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// fsyncDir makes the rename durable. Losing the latest signed tree head
// would weaken the transparency-log consistency check after a crash.
func fsyncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}
