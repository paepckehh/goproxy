// Package store implements the on-disk module cache and its in-memory index.
//
// The disk layout mirrors the standard GOPROXY download layout so the cache
// doubles as a file:// proxy source for the go tool:
//
//	<root>/download/<escaped module>/@v/<escaped version>.{info,mod,zip}
//	<root>/download/<escaped module>/@v/<escaped version>.sum   signed checksum record
//
// Module zips are stored exactly as received from upstream: they are already
// deflate-compressed, which is the space-efficient form. The index lives
// entirely in memory and is rebuilt by Scan at startup.
package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	"golang.org/x/sync/errgroup"
)

// Kind identifies a cached module file kind.
type Kind string

const (
	KindInfo Kind = "info"
	KindMod  Kind = "mod"
	KindZip  Kind = "zip"
)

// ErrNotFound reports a cache miss.
var ErrNotFound = errors.New("store: not found")

// File is one cached file plus its hot access counters.
type File struct {
	Path      string
	Size      int64
	Hash      string // checksum-database hash, base64 without the h1: prefix
	Verified  bool   // hash was checked against the signed record
	FetchedAt time.Time

	requests atomic.Int64
	served   atomic.Int64
	lastUsed atomic.Int64 // unix nanoseconds
}

// Requests returns how many times the file was served.
func (f *File) Requests() int64 { return f.requests.Load() }

// Served returns the total bytes served for this file.
func (f *File) Served() int64 { return f.served.Load() }

// LastUsed returns the time of the last hit, or the zero time.
func (f *File) LastUsed() time.Time {
	if n := f.lastUsed.Load(); n != 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}

// RecordHit accounts one served response of the given size.
func (f *File) RecordHit(size int64) {
	f.requests.Add(1)
	f.served.Add(size)
	f.lastUsed.Store(time.Now().UnixNano())
}

// Open opens the cached file for reading.
func (f *File) Open() (*os.File, error) { return os.Open(f.Path) }

// Version groups the cached files of one module version.
type Version struct {
	Module  string
	Version string
	Info    *File
	Mod     *File
	Zip     *File
	Record  string // signed checksum database note, kept as provenance
}

// Module groups all cached versions of one module.
type Module struct {
	Path     string
	Versions map[string]*Version
}

// Store is a disk-backed module cache with an in-memory index.
type Store struct {
	root string
	mu   sync.RWMutex
	mods map[string]*Module
}

// New creates the store directory tree and returns an empty index.
// Call Scan to fill the index from disk.
func New(root string) (*Store, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	for _, dir := range []string{"download", "tmp"} {
		if err := os.MkdirAll(filepath.Join(abs, dir), 0o755); err != nil {
			return nil, fmt.Errorf("store: create %s: %w", dir, err)
		}
	}
	return &Store{root: abs, mods: make(map[string]*Module)}, nil
}

// Root returns the absolute cache root.
func (s *Store) Root() string { return s.root }

func (s *Store) path(mod, ver string, kind Kind) (string, error) {
	esc, err := module.EscapePath(mod)
	if err != nil {
		return "", fmt.Errorf("store: %w", err)
	}
	escv, err := module.EscapeVersion(ver)
	if err != nil {
		return "", fmt.Errorf("store: %w", err)
	}
	return filepath.Join(s.root, "download", esc, "@v", escv+"."+string(kind)), nil
}

// Create opens a temp file inside the cache for staging a download.
// Commit it with Put or PutBytes.
func (s *Store) Create() (*os.File, error) {
	return os.CreateTemp(filepath.Join(s.root, "tmp"), "dl-*")
}

// Put atomically moves a downloaded, verified temp file into the cache and
// indexes it. tmpPath must have been created with Create.
func (s *Store) Put(mod, ver string, kind Kind, tmpPath, hash string, verified bool) (*File, error) {
	if err := validate(mod, ver, kind); err != nil {
		return nil, err
	}
	final, err := s.path(mod, ver, kind)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return nil, fmt.Errorf("store: mkdir: %w", err)
	}
	fi, err := os.Stat(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if err := os.Rename(tmpPath, final); err != nil {
		return nil, fmt.Errorf("store: commit: %w", err)
	}
	fsyncDir(filepath.Dir(final))
	f := &File{
		Path:      final,
		Size:      fi.Size(),
		Hash:      strings.TrimPrefix(hash, "h1:"),
		Verified:  verified,
		FetchedAt: time.Now().UTC(),
	}
	s.addFile(mod, ver, kind, f)
	return f, nil
}

// PutBytes caches a small in-memory payload such as .info or .mod.
func (s *Store) PutBytes(mod, ver string, kind Kind, data []byte, hash string, verified bool) (*File, error) {
	tmp, err := s.Create()
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name()) // no-op once Put renamed it
	_, werr := tmp.Write(data)
	serr := tmp.Sync()
	cerr := tmp.Close()
	if werr != nil || serr != nil || cerr != nil {
		return nil, errors.Join(werr, serr, cerr)
	}
	return s.Put(mod, ver, kind, tmp.Name(), hash, verified)
}

// PutRecord stores the signed checksum database record for module@version.
func (s *Store) PutRecord(mod, ver, record string) error {
	if err := validate(mod, ver, KindMod); err != nil {
		return err
	}
	path, err := s.path(mod, ver, Kind("sum"))
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path, []byte(record)); err != nil {
		return fmt.Errorf("store: record: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version(mod, ver).Record = record
	return nil
}

// Get returns the indexed file, or nil on a miss.
func (s *Store) Get(mod, ver string, kind Kind) *File {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := s.mods[mod]
	if m == nil {
		return nil
	}
	v := m.Versions[ver]
	if v == nil {
		return nil
	}
	var f *File
	switch kind {
	case KindInfo:
		f = v.Info
	case KindMod:
		f = v.Mod
	case KindZip:
		f = v.Zip
	}
	return f
}

// Open returns the file to serve, falling back to a disk stat when the file
// is present but not yet indexed (startup scan race, restored index).
func (s *Store) Open(mod, ver string, kind Kind) (*File, error) {
	if f := s.Get(mod, ver, kind); f != nil {
		return f, nil
	}
	path, err := s.path(mod, ver, kind)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: %w", err)
	}
	f := &File{Path: path, Size: fi.Size(), FetchedAt: fi.ModTime().UTC()}
	s.addFile(mod, ver, kind, f)
	return f, nil
}

// Summary holds aggregate cache statistics for the dashboard.
type Summary struct {
	Modules  int   `json:"modules"`
	Versions int   `json:"versions"`
	Files    int   `json:"files"`
	Verified int   `json:"verified"`
	Size     int64 `json:"sizeBytes"`
}

func (s *Store) Summary() Summary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	su := Summary{Modules: len(s.mods)}
	for _, m := range s.mods {
		su.Versions += len(m.Versions)
		for _, v := range m.Versions {
			for _, f := range v.files() {
				su.Files++
				su.Size += f.Size
				if f.Verified {
					su.Verified++
				}
			}
		}
	}
	return su
}

// ModStat aggregates one module for the dashboard.
type ModStat struct {
	Path     string    `json:"path"`
	Versions int       `json:"versions"`
	Size     int64     `json:"sizeBytes"`
	Requests int64     `json:"requests"`
	Served   int64     `json:"servedBytes"`
	LastUsed time.Time `json:"lastUsed"`
	Verified bool      `json:"verified"`
}

// Modules returns per-module aggregates, sorted by bytes served descending.
func (s *Store) Modules() []ModStat {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ModStat, 0, len(s.mods))
	for _, m := range s.mods {
		ms := ModStat{Path: m.Path, Versions: len(m.Versions)}
		for _, v := range m.Versions {
			for _, f := range v.files() {
				ms.Size += f.Size
				ms.Requests += f.Requests()
				ms.Served += f.Served()
				if f.Verified {
					ms.Verified = true
				}
				if lu := f.LastUsed(); lu.After(ms.LastUsed) {
					ms.LastUsed = lu
				}
			}
		}
		out = append(out, ms)
	}
	slices.SortFunc(out, func(a, b ModStat) int { return cmp.Compare(b.Served, a.Served) })
	return out
}

// Scan rebuilds the in-memory index by walking the cache directory.
// Version dirs are scanned with bounded parallelism.
func (s *Store) Scan(ctx context.Context) (modules int, err error) {
	dlDir := filepath.Join(s.root, "download")
	var dirs []string
	err = walkDirs(ctx, dlDir, &dirs)
	if err != nil {
		return 0, fmt.Errorf("store: scan: %w", err)
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(8)
	for _, dir := range dirs {
		g.Go(func() error { return s.scanVersionDir(gctx, dir) })
	}
	if err := g.Wait(); err != nil {
		return 0, fmt.Errorf("store: scan: %w", err)
	}
	s.mu.RLock()
	modules = len(s.mods)
	s.mu.RUnlock()
	return modules, nil
}

func (s *Store) scanVersionDir(ctx context.Context, dir string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// dir is the @v dir; the module path is its parent relative to download/.
	rel, err := filepath.Rel(filepath.Join(s.root, "download"), filepath.Dir(dir))
	if err != nil {
		return nil
	}
	mod, err := module.UnescapePath(filepath.ToSlash(rel))
	if err != nil {
		return nil // foreign directory, skip
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	// Records first so file hashes can be backfilled while indexing.
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sum") {
			continue
		}
		ver, err := module.UnescapeVersion(strings.TrimSuffix(e.Name(), ".sum"))
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.version(mod, ver).Record = string(data)
		s.mu.Unlock()
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		dot := strings.LastIndexByte(e.Name(), '.')
		if dot < 0 {
			continue
		}
		stem, ext := e.Name()[:dot], e.Name()[dot+1:]
		switch Kind(ext) {
		case KindInfo, KindMod, KindZip:
			ver, err := module.UnescapeVersion(stem)
			if err != nil || !semver.IsValid(ver) {
				continue
			}
			fi, err := e.Info()
			if err != nil {
				continue
			}
			f := &File{Path: filepath.Join(dir, e.Name()), Size: fi.Size(), FetchedAt: fi.ModTime().UTC()}
			s.addFile(mod, ver, Kind(ext), f)
		}
	}
	return nil
}

// walkDirs collects every directory named @v below root.
func walkDirs(ctx context.Context, root string, dirs *[]string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "@v" {
			*dirs = append(*dirs, path)
			return fs.SkipDir
		}
		return nil
	})
}

// addFile indexes f, backfilling its hash from a stored record if present.
func (s *Store) addFile(mod, ver string, kind Kind, f *File) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.version(mod, ver)
	switch kind {
	case KindInfo:
		v.Info = f
	case KindMod:
		v.Mod = f
	case KindZip:
		v.Zip = f
	}
	if v.Record != "" && kind != KindInfo {
		modHash, zipHash, _ := parseRecordHashes(v.Record, mod, ver)
		if kind == KindMod && modHash != "" {
			f.Hash, f.Verified = modHash, true
		}
		if kind == KindZip && zipHash != "" {
			f.Hash, f.Verified = zipHash, true
		}
	}
}

// version returns (creating if needed) the version entry. Callers hold mu.
func (s *Store) version(mod, ver string) *Version {
	m := s.mods[mod]
	if m == nil {
		m = &Module{Path: mod, Versions: make(map[string]*Version)}
		s.mods[mod] = m
	}
	v := m.Versions[ver]
	if v == nil {
		v = &Version{Module: mod, Version: ver}
		m.Versions[ver] = v
	}
	return v
}

// files returns the version's cached files, skipping absent kinds.
func (v *Version) files() []*File {
	files := make([]*File, 0, 3)
	for _, f := range []*File{v.Info, v.Mod, v.Zip} {
		if f != nil {
			files = append(files, f)
		}
	}
	return files
}

func validate(mod, ver string, kind Kind) error {
	if kind != KindInfo && kind != KindMod && kind != KindZip {
		return fmt.Errorf("store: invalid kind %q", kind)
	}
	if err := module.CheckPath(mod); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if !semver.IsValid(ver) {
		return fmt.Errorf("store: invalid version %q", ver)
	}
	return nil
}

// parseRecordHashes extracts the go.sum h1 hashes from a signed record.
func parseRecordHashes(record, mod, ver string) (modHash, zipHash string, ok bool) {
	for _, line := range strings.Split(record, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[0] != mod || !strings.HasPrefix(fields[2], "h1:") {
			continue
		}
		ok = true
		h := strings.TrimPrefix(fields[2], "h1:")
		switch fields[1] {
		case ver:
			zipHash = h
		case ver + "/go.mod":
			modHash = h
		}
	}
	return modHash, zipHash, ok
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
	// Sync before the rename below: a torn record sidecar would poison
	// the backfilled hashes of every later index scan.
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
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	fsyncDir(dir)
	return nil
}

func fsyncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}
