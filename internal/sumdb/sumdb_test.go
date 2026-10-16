package sumdb

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	db := newTestDB(t, func(path, ver string) ([]byte, error) {
		return nil, errors.New("not found")
	})

	_, err := db.Lookup("example.com/unknown", "v1.0.0")
	if err == nil {
		t.Fatal("expected error for unknown module")
	}
	t.Logf("unknown lookup err: %v (ErrUnknown match: %v)", err, errors.Is(err, ErrUnknown))
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
		t.Errorf("err = %v, want not-found marker", err)
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
