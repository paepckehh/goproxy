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
	"time"

	"golang.org/x/mod/module"
	"golang.org/x/mod/sumdb"
	"golang.org/x/mod/sumdb/dirhash"
	"golang.org/x/mod/sumdb/note"
)

// DefaultKey is the public key of sum.golang.org.
const DefaultKey = "sum.golang.org+033de0ae+Ac4zctda0e5eza+HJyk9SxEdh+s3Ux18htTTAD8OuAn8"

// ErrUnknown reports a module version not present in the checksum database.
var ErrUnknown = errors.New("sumdb: module not in checksum database")

// notFoundMarker is embedded in ReadRemote 404 errors; the x/mod client
// wraps errors with %v, so the marker is matched as text in Lookup.
const notFoundMarker = "not found"

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
	base   *url.URL
	key    string
	name   string
	dir    string
	http   *http.Client
	log    *slog.Logger
	client *sumdb.Client
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
	db := &DB{base: u, key: key, name: verifier.Name(), dir: stateDir, http: client, log: log}
	db.client = sumdb.NewClient(db)
	return db, nil
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
	zipLines, err := db.client.Lookup(mod, ver)
	if err != nil {
		// The x/mod client wraps errors with %v, breaking errors.Is;
		// the "not found" marker is only produced by our own ReadRemote.
		if strings.Contains(err.Error(), notFoundMarker) {
			return nil, ErrUnknown
		}
		return nil, err
	}
	modLines, err := db.client.Lookup(mod, ver+"/go.mod")
	if err != nil {
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
		return nil, fmt.Errorf("sumdb: %s: %s", path, notFoundMarker)
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
	return writeFileAtomic(path, new)
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
