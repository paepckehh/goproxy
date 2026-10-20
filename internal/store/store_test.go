package store

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// testZip builds a module zip with the protocol-mandated file prefix.
func testZip(t *testing.T, mod, ver string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(mod + "@" + ver + "/go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("module " + mod + "\n\ngo 1.21\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStore_PutBytesRoundtrip(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	f, err := s.PutBytes("example.com/m", "v1.0.0", KindMod, []byte("module example.com/m\n"), "h1:abc", true)
	if err != nil {
		t.Fatal(err)
	}
	if f.Size != int64(len("module example.com/m\n")) {
		t.Errorf("size = %d, want %d", f.Size, len("module example.com/m"))
	}

	got, err := s.Open("example.com/m", "v1.0.0", KindMod)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != f.Path {
		t.Errorf("path mismatch: %q vs %q", got.Path, f.Path)
	}
}

func TestStore_PutZipAndLayout(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	data := testZip(t, "example.com/m", "v1.2.3")
	tmp, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.Write(data); err != nil {
		t.Fatal(err)
	}
	name := tmp.Name()
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := s.Put("example.com/m", "v1.2.3", KindZip, name, "h1:ziphash", true)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(s.Root(), "download", "example.com", "m", "@v", "v1.2.3.zip")
	if f.Path != want {
		t.Errorf("zip path = %q, want %q", f.Path, want)
	}
	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Error("temp file still exists after Put")
	}
}

func TestStore_ValidateRejectsBadInput(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	cases := []struct {
		name    string
		mod     string
		ver     string
		wantErr bool
	}{
		{"valid", "example.com/m", "v1.0.0", false},
		{"pseudo version", "example.com/m", "v0.0.0-20200101000000-abcdefabcdef", false},
		{"prerelease", "example.com/m", "v1.0.0-beta.1", false},
		{"bad version", "example.com/m", "latest", true},
		{"empty version", "example.com/m", "", true},
		{"traversal", "../../../etc", "v1.0.0", true},
		{"bad module", "!!!", "v1.0.0", true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := s.PutBytes(tt.mod, tt.ver, KindInfo, []byte("{}"), "", false)
			if (err != nil) != tt.wantErr {
				t.Errorf("PutBytes(%q, %q) err = %v, wantErr %v", tt.mod, tt.ver, err, tt.wantErr)
			}
		})
	}
}

func TestStore_OpenMiss(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if _, err := s.Open("example.com/m", "v1.0.0", KindZip); err != ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestStore_OpenDiskFallback(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	// File on disk but not in the index (as during the startup scan).
	f, err := s.PutBytes("example.com/m", "v1.0.0", KindMod, []byte("module m\n"), "", false)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.mods = make(map[string]*Module)
	s.mu.Unlock()

	got, err := s.Open("example.com/m", "v1.0.0", KindMod)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != f.Path || got.Size != f.Size {
		t.Errorf("disk fallback = %+v, want path %q size %d", got, f.Path, f.Size)
	}
}

func TestStore_RecordAndScanRebuild(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	const mod, ver = "example.com/m", "v1.0.0"
	modBytes := []byte("module example.com/m\n")
	record := "example.com/m v1.0.0 h1:ziphash1=\nexample.com/m v1.0.0/go.mod h1:modhash1=\n"

	if _, err := s.PutBytes(mod, ver, KindMod, modBytes, "h1:modhash1=", true); err != nil {
		t.Fatal(err)
	}
	zipBytes := testZip(t, mod, ver)
	tmp, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.Write(zipBytes); err != nil {
		t.Fatal(err)
	}
	name := tmp.Name()
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(mod, ver, KindZip, name, "h1:ziphash1=", true); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRecord(mod, ver, record); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutBytes(mod, ver, KindInfo, []byte(`{"Version":"v1.0.0"}`), "", false); err != nil {
		t.Fatal(err)
	}

	su := s.Summary()
	if su.Modules != 1 || su.Versions != 1 || su.Files != 3 || su.Verified != 2 {
		t.Errorf("summary = %+v, want 1 module, 1 version, 3 files, 2 verified", su)
	}

	// Rebuild the index from disk in a fresh store sharing the same root.
	fresh, err := New(s.Root())
	if err != nil {
		t.Fatal(err)
	}
	n, err := fresh.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("scan found %d modules, want 1", n)
	}
	su2 := fresh.Summary()
	if su2 != su {
		t.Errorf("scanned summary %+v != original %+v", su2, su)
	}
	zip := fresh.Get(mod, ver, KindZip)
	if zip == nil || !zip.Verified || zip.Hash != "ziphash1=" {
		t.Errorf("scanned zip entry = %+v, want verified with hash ziphash1=", zip)
	}
	mf := fresh.Get(mod, ver, KindMod)
	if mf == nil || !mf.Verified || mf.Hash != "modhash1=" {
		t.Errorf("scanned mod entry = %+v, want verified with hash modhash1=", mf)
	}
}

func TestStore_ModulesStats(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	if _, err := s.PutBytes("a.example/x", "v1.0.0", KindMod, []byte("module a\n"), "", false); err != nil {
		t.Fatal(err)
	}
	f, err := s.Open("a.example/x", "v1.0.0", KindMod)
	if err != nil {
		t.Fatal(err)
	}
	f.RecordHit(100)
	f.RecordHit(50)

	ms := s.Modules()
	if len(ms) != 1 {
		t.Fatalf("modules = %d, want 1", len(ms))
	}
	if ms[0].Requests != 2 || ms[0].Served != 150 {
		t.Errorf("stats = %+v, want 2 requests, 150 bytes served", ms[0])
	}
	if ms[0].LastUsed.IsZero() {
		t.Error("last used not tracked")
	}

	usage := s.TakeUsage()
	if got := usage["a.example/x"]; got.Requests != 2 || got.Served != 150 {
		t.Fatalf("take usage = %+v, want 2 requests, 150 bytes", got)
	}
	if again := s.TakeUsage(); len(again) != 0 {
		t.Errorf("second take = %+v, want no unchanged modules", again)
	}
}

func TestStore_UsageSurvivesRestart(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	if _, err := s.PutBytes("a.example/x", "v1.0.0", KindZip, []byte("zip\n"), "", true); err != nil {
		t.Fatal(err)
	}
	f, err := s.Open("a.example/x", "v1.0.0", KindZip)
	if err != nil {
		t.Fatal(err)
	}
	f.RecordHit(4096)

	fresh, err := New(s.Root())
	if err != nil {
		t.Fatal(err)
	}
	fresh.RestoreUsage(s.TakeUsage())
	if _, err := fresh.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	ms := fresh.Modules()
	if len(ms) != 1 || ms[0].Requests != 1 || ms[0].Served != 4096 {
		t.Fatalf("restored modules = %+v, want 1 request, 4096 bytes served", ms)
	}
	f2, err := fresh.Open("a.example/x", "v1.0.0", KindZip)
	if err != nil {
		t.Fatal(err)
	}
	f2.RecordHit(1024)
	if ms = fresh.Modules(); ms[0].Served != 5120 {
		t.Errorf("served after restore = %d, want 5120", ms[0].Served)
	}
}

func TestStore_ConcurrentAccess(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			const mod, ver = "example.com/m", "v1.0.0"
			if _, err := s.PutBytes(mod, ver, KindInfo, []byte("{}"), "", false); err != nil {
				t.Error(err)
				return
			}
			if _, err := s.Open(mod, ver, KindInfo); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if f := s.Get("example.com/m", "v1.0.0", KindInfo); f == nil {
		t.Error("concurrent writes lost the file")
	}
}
