package ui

import (
	"context"
	"fmt"
	"math"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"paepcke.de/goproxy/internal/clients"
	"paepcke.de/goproxy/internal/fleet"
	"paepcke.de/goproxy/internal/history"
	"paepcke.de/goproxy/internal/stats"
	"paepcke.de/goproxy/internal/store"
)

// TestScreenshots renders the dashboard against a deterministic demo
// archive and writes the images referenced by README.md. It is opt-in
// because it needs a local Chromium/Chrome binary:
//
//	GOPROXY_SCREENSHOTS=1 go test ./internal/ui -run TestScreenshots -v
func TestScreenshots(t *testing.T) {
	if os.Getenv("GOPROXY_SCREENSHOTS") != "1" {
		t.Skip("set GOPROXY_SCREENSHOTS=1 to render the README screenshots")
	}
	chromium, ok := findChromium()
	if !ok {
		t.Fatal("Chromium not found: install it or set GOPROXY_CHROMIUM")
	}
	nodePath, ok := playwrightNodePath()
	if !ok {
		t.Fatal("Playwright not found: npm install playwright or set GOPROXY_PLAYWRIGHT_NODE_PATH")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal("node not found: install Node.js to render screenshots")
	}
	script, err := filepath.Abs(filepath.Join("testdata", "screenshot.cjs"))
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	modules := demoModules(now)
	st := demoStore(t, modules)
	hist := demoHistory(t, now, st.Summary().Size)
	tracker := demoClients(now)
	m := demoStats(t, modules)
	peers := fakePeers{
		{URL: "http://goproxy01:9090", Online: true, Version: "v0.1.105", LatencyMS: 3, CheckedAt: now.Add(-38 * time.Second)},
		{URL: "http://goproxy02:9090", Online: true, Version: "v0.1.105", LatencyMS: 5, CheckedAt: now.Add(-38 * time.Second)},
		{URL: "http://goproxy03:9090", Online: true, Version: "v0.1.104", LatencyMS: 9, CheckedAt: now.Add(-39 * time.Second)},
		{URL: "http://goproxy04:9090", Online: false, Err: "healthz status 503", CheckedAt: now.Add(-38 * time.Second)},
	}
	report := fleet.WarmReport{
		StartedAt: now.Add(-18 * time.Minute), DurationMS: 41200, NextRun: now.Add(41 * time.Minute),
		Modules: 100, Fetched: 12, Bytes: 3_720_000_000, PeerBytes: 2_240_000_000, Failed: 1,
		Items: []fleet.WarmItem{
			{Module: "golang.org/x/mod", Version: "v0.42.0", Bytes: 512 << 20, PeerBytes: 512 << 20, Source: "goproxy02:9090"},
			{Module: "google.golang.org/grpc", Version: "v1.76.0", Bytes: 488 << 20, PeerBytes: 488 << 20, Source: "goproxy01:9090"},
			{Module: "k8s.io/client-go", Version: "v0.34.1", Bytes: 620 << 20, PeerBytes: 620 << 20, Source: "goproxy03:9090"},
			{Module: "github.com/prometheus/prometheus", Version: "v0.307.0", Bytes: 590 << 20},
			{Module: "golang.org/x/tools", Version: "v0.36.0", Bytes: 440 << 20, PeerBytes: 440 << 20, Source: "goproxy02:9090"},
			{Module: "github.com/stretchr/testify", Version: "v1.11.1"},
			{Module: "gopkg.in/yaml.v3", Version: "v3.0.1"},
			{Module: "forgejo.adm.corp/libs/buildinfo", Version: "v0.4.2", Err: "upstream 404: private module"},
		},
	}
	h := New(m, st, hist, tracker, "v0.1.106", peers, fakePrewarm(report))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	dir := filepath.Join("..", "..", "docs", "screenshots")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	shots := []struct {
		name   string
		url    string
		width  int
		height int
		wait   string
		scroll string
		full   bool
	}{
		{"dashboard-hero.jpg", srv.URL + "/", 1440, 1050, "#modules-body tr", "", false},
		{"dashboard-tables.jpg", srv.URL + "/#popular", 1440, 1050, "#clients-req-body tr", "#popular", false},
		{"dashboard-history.jpg", srv.URL + "/#history", 1440, 1050, "#history-summary .hstat", "#history", false},
		{"dashboard-mobile.jpg", srv.URL + "/", 430, 932, "#modules-body tr", "", false},
		{"dashboard-full.jpg", srv.URL + "/", 1440, 1050, "#modules-body tr", "", true},
	}
	for _, shot := range shots {
		runScreenshot(t, script, nodePath, chromium, shot.url,
			filepath.Join(dir, shot.name), shot.width, shot.height, shot.wait, shot.scroll, shot.full)
	}
}

func runScreenshot(t *testing.T, script, nodePath, chromium, url, out string, width, height int, wait, scroll string, full bool) {
	t.Helper()
	fullPage := "0"
	if full {
		fullPage = "1"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", script, url, out,
		fmt.Sprint(width), fmt.Sprint(height), wait, scroll, fullPage)
	cmd.Env = append(os.Environ(), "NODE_PATH="+nodePath, "GOPROXY_CHROMIUM="+chromium)
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("screenshot %s: %v\n%s", filepath.Base(out), err, b)
	}
	fi, err := os.Stat(out)
	if err != nil {
		t.Fatalf("screenshot %s: %v\n%s", filepath.Base(out), err, b)
	}
	if fi.Size() < 8<<10 {
		t.Fatalf("screenshot %s is unexpectedly small (%d bytes):\n%s", filepath.Base(out), fi.Size(), b)
	}
	t.Logf("wrote %s (%d bytes)", out, fi.Size())
}

func findChromium() (string, bool) {
	if p := os.Getenv("GOPROXY_CHROMIUM"); p != "" {
		return p, true
	}
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, true
		}
	}
	for _, p := range []string{
		"/run/current-system/sw/bin/chromium",
		"/usr/bin/chromium",
		"/usr/bin/google-chrome",
	} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, true
		}
	}
	return "", false
}

func playwrightNodePath() (string, bool) {
	if p := os.Getenv("GOPROXY_PLAYWRIGHT_NODE_PATH"); p != "" {
		return p, true
	}
	for _, base := range []string{".", "..", "../..", "../../.."} {
		nodePath, err := filepath.Abs(filepath.Join(base, "node_modules"))
		if err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(nodePath, "playwright")); err == nil {
			return nodePath, true
		}
	}
	home, err := os.UserHomeDir()
	if err == nil {
		matches, _ := filepath.Glob(filepath.Join(home, ".npm", "_npx", "*", "node_modules", "playwright", "package.json"))
		for _, match := range matches {
			nodePath := filepath.Dir(filepath.Dir(match))
			if _, err := os.Stat(filepath.Join(nodePath, "playwright")); err == nil {
				return nodePath, true
			}
		}
	}
	if out, err := exec.Command("npm", "root", "-g").Output(); err == nil {
		nodePath := strings.TrimSpace(string(out))
		if _, err := os.Stat(filepath.Join(nodePath, "playwright")); err == nil {
			return nodePath, true
		}
	}
	return "", false
}

type demoVersion struct {
	version string
	sizeMB  int
}

type demoModule struct {
	path     string
	versions []demoVersion
	requests int64
	served   int64
	lastUsed time.Time
	verified bool
}

func demoModules(now time.Time) []demoModule {
	return []demoModule{
		{path: "golang.org/x/mod", versions: []demoVersion{{"v0.42.0", 124}, {"v0.41.0", 98}, {"v0.40.0", 96}}, requests: 48920, served: 412 << 30, lastUsed: now.Add(-2 * time.Minute), verified: true},
		{path: "github.com/gin-gonic/gin", versions: []demoVersion{{"v1.11.0", 96}, {"v1.10.1", 92}}, requests: 42110, served: 361 << 30, lastUsed: now.Add(-5 * time.Minute), verified: true},
		{path: "google.golang.org/grpc", versions: []demoVersion{{"v1.76.0", 148}, {"v1.75.0", 142}, {"v1.74.0", 138}}, requests: 37440, served: 338 << 30, lastUsed: now.Add(-9 * time.Minute), verified: true},
		{path: "k8s.io/client-go", versions: []demoVersion{{"v0.34.1", 212}, {"v0.34.0", 208}}, requests: 32180, served: 286 << 30, lastUsed: now.Add(-14 * time.Minute), verified: true},
		{path: "github.com/prometheus/prometheus", versions: []demoVersion{{"v0.307.0", 224}, {"v0.306.0", 219}}, requests: 28910, served: 241 << 30, lastUsed: now.Add(-21 * time.Minute), verified: true},
		{path: "golang.org/x/tools", versions: []demoVersion{{"v0.36.0", 156}, {"v0.35.0", 151}}, requests: 26140, served: 198 << 30, lastUsed: now.Add(-33 * time.Minute), verified: true},
		{path: "github.com/stretchr/testify", versions: []demoVersion{{"v1.11.1", 54}, {"v1.10.0", 52}, {"v1.9.0", 50}}, requests: 23770, served: 152 << 30, lastUsed: now.Add(-47 * time.Minute), verified: true},
		{path: "gopkg.in/yaml.v3", versions: []demoVersion{{"v3.0.1", 38}}, requests: 21430, served: 118 << 30, lastUsed: now.Add(-64 * time.Minute), verified: true},
		{path: "github.com/spf13/cobra", versions: []demoVersion{{"v1.10.1", 44}, {"v1.9.1", 42}}, requests: 19220, served: 91 << 30, lastUsed: now.Add(-82 * time.Minute), verified: true},
		{path: "github.com/google/uuid", versions: []demoVersion{{"v1.6.0", 18}}, requests: 17480, served: 64 << 30, lastUsed: now.Add(-111 * time.Minute), verified: true},
		{path: "golang.org/x/sync", versions: []demoVersion{{"v0.24.0", 32}, {"v0.23.0", 31}}, requests: 15330, served: 48 << 30, lastUsed: now.Add(-133 * time.Minute), verified: true},
		{path: "modernc.org/sqlite", versions: []demoVersion{{"v1.60.1", 318}, {"v1.59.0", 312}}, requests: 13840, served: 39 << 30, lastUsed: now.Add(-161 * time.Minute), verified: true},
		{path: "github.com/klauspost/compress", versions: []demoVersion{{"v1.18.1", 86}}, requests: 11240, served: 31 << 30, lastUsed: now.Add(-190 * time.Minute), verified: true},
		{path: "forgejo.adm.corp/libs/buildinfo", versions: []demoVersion{{"v0.4.2", 26}}, requests: 26400, served: 128 << 30, lastUsed: now.Add(-11 * time.Minute)},
	}
}

func demoStore(t *testing.T, modules []demoModule) *store.Store {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, md := range modules {
		for _, v := range md.versions {
			info := []byte(fmt.Sprintf(`{"Version":%q,"Time":"2026-10-18T12:00:00Z"}`, v.version))
			putDemoFile(t, st, md.path, v.version, store.KindInfo, info, 96, false)
			modBytes := []byte("module " + md.path + "\n\ngo 1.27\n")
			putDemoFile(t, st, md.path, v.version, store.KindMod, modBytes, 2<<10, md.verified)
			zipBytes := []byte("demo archive for " + md.path + "@" + v.version)
			putDemoFile(t, st, md.path, v.version, store.KindZip, zipBytes, int64(v.sizeMB)<<20, md.verified)
		}
	}
	usage := make(map[string]store.ModuleUsage, len(modules))
	for _, md := range modules {
		usage[md.path] = store.ModuleUsage{Requests: md.requests, Served: md.served, LastUsed: md.lastUsed}
	}
	st.RestoreUsage(usage)
	return st
}

func putDemoFile(t *testing.T, st *store.Store, mod, ver string, kind store.Kind, data []byte, size int64, verified bool) {
	t.Helper()
	f, err := st.PutBytes(mod, ver, kind, data, "", verified)
	if err != nil {
		t.Fatal(err)
	}
	f.Size = size
	f.FetchedAt = time.Now().UTC().Add(-time.Duration(size%72) * time.Hour)
}

func demoHistory(t *testing.T, now time.Time, storeEnd int64) *history.DB {
	t.Helper()
	db, err := history.Open(filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	end := now.Truncate(time.Minute)
	start := end.Add(-365 * 24 * time.Hour)
	startBytes := storeEnd * 3 / 5
	add := func(ts time.Time, minutes int) {
		progress := float64(ts.Sub(start)) / float64(end.Sub(start))
		hour := float64(ts.Hour()) + float64(ts.Minute())/60
		day := 0.55 + 0.45*math.Sin((hour-6)/24*2*math.Pi)
		weekday := 1.0
		if ts.Weekday() == time.Saturday || ts.Weekday() == time.Sunday {
			weekday = 0.58
		}
		jitter := 0.86 + 0.28*math.Mod(float64(ts.Unix()/int64(minutes*60))/17, 1)
		rate := 160 * day * weekday * jitter
		requests := int64(rate * float64(minutes))
		hits := requests * 91 / 100
		misses := requests - hits
		bucket := stats.Bucket{
			Bucket: ts,
			Totals: stats.Totals{
				Requests: requests, Hits: hits, Misses: misses,
				Passes:          requests / 80,
				Verifications:   misses,
				VerifyFailures:  requests / 90000,
				BytesDown:       requests * 9 << 20,
				BytesUp:         misses * 65 << 20,
				BytesSaved:      hits * 9 << 20,
				BytesNeighbour:  hits * 9 << 20 / 28,
				NeighbourHits:   hits / 40,
				NeighbourMisses: misses / 2,
			},
			Store: stats.StoreStat{
				Bytes:    startBytes + int64(progress*float64(storeEnd-startBytes)),
				Modules:  int64(8 + progress*6),
				Versions: int64(16 + progress*11),
				Files:    int64(40 + progress*41),
			},
		}
		if err := db.RecordBucket(bucket); err != nil {
			t.Fatal(err)
		}
	}
	for ts := end.Add(-24 * time.Hour); !ts.After(end); ts = ts.Add(5 * time.Minute) {
		add(ts, 5)
	}
	for ts := end.Add(-30 * 24 * time.Hour); ts.Before(end.Add(-24 * time.Hour)); ts = ts.Add(time.Hour) {
		add(ts, 60)
	}
	for ts := end.Add(-365 * 24 * time.Hour); ts.Before(end.Add(-30 * 24 * time.Hour)); ts = ts.Add(6 * time.Hour) {
		add(ts, 360)
	}
	return db
}

func demoClients(now time.Time) *clients.Tracker {
	tr := clients.New()
	tr.Restore(map[string]clients.Usage{
		"10.42.0.17":  {Requests: 184210, BytesDown: 1_920_000_000_000, LastSeen: now.Add(-90 * time.Second)},
		"10.42.7.3":   {Requests: 142340, BytesDown: 2_640_000_000_000, LastSeen: now.Add(-3 * time.Minute)},
		"10.42.2.44":  {Requests: 121880, BytesDown: 1_180_000_000_000, LastSeen: now.Add(-6 * time.Minute)},
		"10.43.0.9":   {Requests: 109420, BytesDown: 940_000_000_000, LastSeen: now.Add(-11 * time.Minute)},
		"10.42.9.201": {Requests: 98410, BytesDown: 1_040_000_000_000, LastSeen: now.Add(-17 * time.Minute)},
		"10.43.4.8":   {Requests: 86330, BytesDown: 780_000_000_000, LastSeen: now.Add(-24 * time.Minute)},
		"10.42.5.120": {Requests: 74210, BytesDown: 620_000_000_000, LastSeen: now.Add(-38 * time.Minute)},
		"10.43.9.2":   {Requests: 65980, BytesDown: 510_000_000_000, LastSeen: now.Add(-56 * time.Minute)},
		"10.42.11.31": {Requests: 55120, BytesDown: 440_000_000_000, LastSeen: now.Add(-74 * time.Minute)},
		"10.43.2.77":  {Requests: 46840, BytesDown: 390_000_000_000, LastSeen: now.Add(-96 * time.Minute)},
		"10.42.14.5":  {Requests: 38110, BytesDown: 310_000_000_000, LastSeen: now.Add(-2 * time.Hour)},
		"10.43.7.19":  {Requests: 29440, BytesDown: 220_000_000_000, LastSeen: now.Add(-3 * time.Hour)},
	})
	return tr
}

func demoStats(t *testing.T, modules []demoModule) *stats.Stats {
	t.Helper()
	m := stats.New(nil,
		stats.WithTickInterval(20*time.Millisecond),
		stats.WithStartedAt(time.Now().Add(-12*24*time.Hour-4*time.Hour)),
	)
	m.Seed(stats.Totals{
		Requests: 1_312_152, Hits: 1_163_402, Misses: 121_510, Passes: 17_240,
		BytesDown: 2_412_000_000_000, BytesUp: 382_000_000_000,
		BytesSaved: 2_030_000_000_000, BytesNeighbour: 118_000_000_000,
		Verifications: 121_510, VerifyFailures: 1,
		NeighbourHits: 32_120, NeighbourMisses: 8_470,
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx)
	}()
	m.Started("v0.1.106", "127.0.0.1:5100")
	m.Indexed(14321)
	for i := range 120 {
		md := modules[i%len(modules)]
		ver := md.versions[len(md.versions)-1].version
		for j := range 8 {
			size := int64(6+(i*7+j*5)%19) << 20
			m.Hit(md.path, ver, "zip", size, time.Duration(6+(i+j)%31)*time.Millisecond)
		}
		if i%3 == 0 {
			size := int64(60+(i%5)*12) << 20
			m.Miss(md.path, ver, "zip", size, size, 38*time.Millisecond, true)
		}
		if i%4 == 0 {
			size := int64(40+(i%4)*10) << 20
			m.NeighbourHit(md.path, ver, "zip", size, size, 17*time.Millisecond, true, "goproxy02:9090")
		}
		if i%10 == 0 {
			m.Pass(md.path, "@latest", 20<<20, 11*time.Millisecond)
		}
		time.Sleep(25 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	m.Prewarm(100, 12, 1, 3_720_000_000, 2_240_000_000)
	m.Maintenance("nightly stats maintenance: 412ms, live db 18.2 MiB, backup stats-20261020T230000-v0.1.106.db.gz (4.1 MiB, 8 kept), semantic check ok")
	m.VerifyFailure("github.com/klauspost/compress", "v1.18.1", "zip hash mismatch: upstream content refused")
	cancel()
	<-done
	return m
}
