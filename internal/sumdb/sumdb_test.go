package sumdb

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/mod/sumdb"
	"golang.org/x/mod/sumdb/note"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// newTestDB spins up a real in-memory checksum database with a random key
// and returns a client pointed at it. gosum supplies the record lines.
func newTestDB(t *testing.T, gosum func(path, ver string) ([]byte, error)) *DB {
	t.Helper()
	skey, vkey, err := note.GenerateKey(rand.Reader, "test.sumdb.example")
	if err != nil {
		t.Fatal(err)
	}
	ts := sumdb.NewTestServer(skey, gosum)
	srv := httptest.NewServer(sumdb.NewServer(ts))
	t.Cleanup(srv.Close)

	db, err := New(srv.URL, vkey, filepath.Join(t.TempDir(), "sumdb-state"), srv.Client(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func recordLines(path, ver string) []byte {
	return []byte(path + " " + ver + " h1:ziphash=\n" +
		path + " " + ver + "/go.mod h1:modhash=\n")
}

func TestDB_LookupVerified(t *testing.T) {
	t.Parallel()
	db := newTestDB(t, func(path, ver string) ([]byte, error) {
		return recordLines(path, ver), nil
	})

	rec, err := db.Lookup("example.com/m", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if rec.ZipHash != "ziphash=" || rec.ModHash != "modhash=" {
		t.Errorf("hashes = %q %q, want ziphash= modhash=", rec.ZipHash, rec.ModHash)
	}
	if rec.Raw == "" {
		t.Error("raw signed record is empty")
	}
}

func TestDB_LookupUnknown(t *testing.T) {
	t.Parallel()
	// os.ErrNotExist maps to 404 on the wire, like sum.golang.org for
	// unknown modules: the client must read that as ErrUnknown.
	// (The test server's reportError matches the raw sentinel only.)
	db := newTestDB(t, func(path, ver string) ([]byte, error) {
		return nil, fs.ErrNotExist
	})

	_, err := db.Lookup("example.com/unknown", "v1.0.0")
	if !errors.Is(err, ErrUnknown) {
		t.Fatalf("err = %v, want ErrUnknown", err)
	}
}

func TestDB_TransientLookupErrorIsRetried(t *testing.T) {
	t.Parallel()
	skey, vkey, err := note.GenerateKey(rand.Reader, "test.sumdb.example")
	if err != nil {
		t.Fatal(err)
	}
	ts := sumdb.NewTestServer(skey, func(path, ver string) ([]byte, error) {
		return recordLines(path, ver), nil
	})
	base := sumdb.NewServer(ts)
	var fail atomic.Bool
	fail.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/lookup/") && fail.CompareAndSwap(true, false) {
			http.Error(w, "temporary outage", http.StatusServiceUnavailable)
			return
		}
		base.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	db, err := New(srv.URL, vkey, filepath.Join(t.TempDir(), "sumdb-state"), srv.Client(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Lookup("example.com/m", "v1.0.0"); err == nil {
		t.Fatal("first lookup succeeded, want the injected outage")
	}
	// The x/mod client caches the failed lookup. A transient outage must
	// not poison this module version for the lifetime of the process.
	if _, err := db.Lookup("example.com/m", "v1.0.0"); err != nil {
		t.Fatalf("lookup did not recover after the transient outage: %v", err)
	}
}

func TestDB_Tile404FailsClosed(t *testing.T) {
	t.Parallel()
	// A database whose lookups answer but whose tiles are missing must
	// read as an outage (fail closed), never as "module unknown".
	skey, vkey, err := note.GenerateKey(rand.Reader, "test.sumdb.example")
	if err != nil {
		t.Fatal(err)
	}
	ts := sumdb.NewTestServer(skey, func(path, ver string) ([]byte, error) {
		return recordLines(path, ver), nil
	})
	sumSrv := sumdb.NewServer(ts)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tile/") {
			http.NotFound(w, r)
			return
		}
		sumSrv.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	db, err := New(srv.URL, vkey, t.TempDir(), srv.Client(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Lookup("example.com/m", "v1.0.0")
	if err == nil {
		t.Fatal("expected error when tiles are unavailable")
	}
	if errors.Is(err, ErrUnknown) {
		t.Fatalf("tile outage misclassified as unknown module (fail-open): %v", err)
	}
}

func TestDB_LookupPartialRecord(t *testing.T) {
	t.Parallel()
	// A record holding only the zip line: the zip hash is passed through,
	// the absent go.mod hash is reported as empty (not refused).
	db := newTestDB(t, func(path, ver string) ([]byte, error) {
		return []byte(path + " " + strings.TrimSuffix(ver, "/go.mod") + " h1:ziphash=\n"), nil
	})

	rec, err := db.Lookup("example.com/m", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if rec.ZipHash != "ziphash=" {
		t.Errorf("zip hash = %q, want ziphash=", rec.ZipHash)
	}
	if rec.ModHash != "" {
		t.Errorf("mod hash = %q, want empty", rec.ModHash)
	}
}

func TestDB_LookupPassesHashesThrough(t *testing.T) {
	t.Parallel()
	db := newTestDB(t, func(path, ver string) ([]byte, error) {
		return []byte(path + " " + ver + " h1:WRONG=\n"), nil
	})

	rec, err := db.Lookup("example.com/m", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if rec.ZipHash != "WRONG=" {
		t.Errorf("zip hash = %q, want WRONG=", rec.ZipHash)
	}
}

func TestDB_BadSignatureFailsClosed(t *testing.T) {
	t.Parallel()
	_, vkey, err := note.GenerateKey(rand.Reader, "test.sumdb.example")
	if err != nil {
		t.Fatal(err)
	}
	evilKey, _, err := note.GenerateKey(rand.Reader, "evil.sumdb.example")
	if err != nil {
		t.Fatal(err)
	}
	ts := sumdb.NewTestServer(evilKey, func(path, ver string) ([]byte, error) {
		return recordLines(path, ver), nil
	})
	srv := httptest.NewServer(sumdb.NewServer(ts))
	t.Cleanup(srv.Close)

	db, err := New(srv.URL, vkey, filepath.Join(t.TempDir(), "sumdb-state"), srv.Client(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Lookup("example.com/m", "v1.0.0"); err == nil {
		t.Fatal("expected verification failure for mismatched key")
	}
}

func TestDB_StatePersistsAcrossRestart(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "sumdb-state")
	skey, vkey, err := note.GenerateKey(rand.Reader, "test.sumdb.example")
	if err != nil {
		t.Fatal(err)
	}
	ts := sumdb.NewTestServer(skey, func(path, ver string) ([]byte, error) {
		return recordLines(path, ver), nil
	})
	srv := httptest.NewServer(sumdb.NewServer(ts))
	t.Cleanup(srv.Close)

	db, err := New(srv.URL, vkey, dir, srv.Client(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Lookup("example.com/m", "v1.0.0"); err != nil {
		t.Fatal(err)
	}

	ts2 := sumdb.NewTestServer(skey, func(path, ver string) ([]byte, error) {
		return recordLines(path, ver), nil
	})
	srv2 := httptest.NewServer(sumdb.NewServer(ts2))
	t.Cleanup(srv2.Close)

	db2, err := New(srv2.URL, vkey, dir, srv2.Client(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db2.Lookup("example.com/m", "v1.0.0"); err != nil {
		t.Fatal(err)
	}
}

func testModZip(t *testing.T, mod, ver string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "m.zip")
	zf, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	w, err := zw.Create(mod + "@" + ver + "/go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("module " + mod + "\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zf.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHashZipAndHashMod(t *testing.T) {
	t.Parallel()
	zipPath := testModZip(t, "example.com/m", "v1.0.0")

	h1, err := HashZip(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix([]byte(h1), []byte("h1:")) {
		t.Errorf("zip hash = %q, want h1: prefix", h1)
	}

	modHash, err := HashMod([]byte("module example.com/m\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix([]byte(modHash), []byte("h1:")) {
		t.Errorf("mod hash = %q, want h1: prefix", modHash)
	}

	other, err := HashMod([]byte("module other\n"))
	if err != nil {
		t.Fatal(err)
	}
	if other == modHash {
		t.Error("different content produced the same hash")
	}
}

func TestNewRejectsBadBase(t *testing.T) {
	t.Parallel()
	for _, base := range []string{"", "not a url", "ftp://x"} {
		if _, err := New(base, DefaultKey, t.TempDir(), nil, nil); err == nil {
			t.Errorf("New(%q) succeeded, want error", base)
		}
	}
}

func TestReadRemote404MapsToNotExist(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	db, err := New(srv.URL, DefaultKey, t.TempDir(), srv.Client(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ReadRemote("/lookup/x@v1")
	if err == nil || !strings.Contains(err.Error(), notFoundMarker) {
		t.Errorf("lookup 404 err = %v, want not-found marker", err)
	}
	_, err = db.ReadRemote("/tile/8/0/x000/000")
	if err == nil || strings.Contains(err.Error(), notFoundMarker) {
		t.Errorf("tile 404 err = %v, want plain status error", err)
	}
}

func TestDefaultKeyParses(t *testing.T) {
	t.Parallel()
	if _, err := note.NewVerifier(DefaultKey); err != nil {
		t.Errorf("default key does not parse: %v", err)
	}
}

func TestDB_HTTPTimeoutApplied(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	db, err := New(srv.URL, DefaultKey, t.TempDir(), &http.Client{Timeout: 20 * time.Millisecond}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ReadRemote("/lookup/x@v1"); err == nil {
		t.Error("expected timeout error")
	}
}

func TestDB_RefreshPersistsVerifiedTreeHead(t *testing.T) {
	t.Parallel()
	db := newTestDB(t, func(path, ver string) ([]byte, error) {
		return recordLines(path, ver), nil
	})
	if _, err := db.Lookup("example.com/m", "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := db.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	head, err := db.ReadConfig(db.name + "/latest")
	if err != nil {
		t.Fatal(err)
	}
	if got := treeSize(head, note.VerifierList(db.verifier)); got < 1 {
		t.Fatalf("persisted tree size = %d, want a signed head", got)
	}
	// A refresh at the same tree is a no-op and must keep working.
	if err := db.Refresh(context.Background()); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	// Verification still works through the replaced client.
	if _, err := db.Lookup("example.com/m", "v1.0.0"); err != nil {
		t.Fatalf("lookup after refresh: %v", err)
	}
}

func TestDB_RefreshRejectsBadSignature(t *testing.T) {
	t.Parallel()
	skey, vkey, err := note.GenerateKey(rand.Reader, "test.sumdb.example")
	if err != nil {
		t.Fatal(err)
	}
	base := sumdb.NewServer(sumdb.NewTestServer(skey, func(path, ver string) ([]byte, error) {
		return recordLines(path, ver), nil
	}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest" {
			// A syntactically valid note signed by another key.
			w.Write([]byte("go.sum database tree\n1\n\n— test.sumdb.example AAAA\n"))
			return
		}
		base.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	db, err := New(srv.URL, vkey, t.TempDir(), srv.Client(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Refresh(context.Background()); err == nil {
		t.Fatal("refresh accepted a tree head with an invalid signature")
	}
}

func TestDB_RefreshLoopStopsOnCancellation(t *testing.T) {
	t.Parallel()
	var latest atomic.Int64
	skey, vkey, err := note.GenerateKey(rand.Reader, "test.sumdb.example")
	if err != nil {
		t.Fatal(err)
	}
	base := sumdb.NewServer(sumdb.NewTestServer(skey, func(path, ver string) ([]byte, error) {
		return recordLines(path, ver), nil
	}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest" {
			latest.Add(1)
		}
		base.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	db, err := New(srv.URL, vkey, t.TempDir(), srv.Client(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		db.RefreshLoop(ctx, 5*time.Millisecond)
	}()
	waitForAtomic(t, &latest)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RefreshLoop did not stop on context cancellation")
	}
}

func waitForAtomic(t *testing.T, n *atomic.Int64) {
	t.Helper()
	for range 500 {
		if n.Load() > 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not reached in time")
}
