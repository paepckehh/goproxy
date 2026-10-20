// Command goproxy is a caching, checksum-verifying Go module proxy with an
// operations dashboard. Point GOPROXY at it and go builds flow through the
// cache; point a browser at the root path for live operations analytics.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"paepcke.de/goproxy/internal/clients"
	"paepcke.de/goproxy/internal/fleet"
	"paepcke.de/goproxy/internal/history"
	"paepcke.de/goproxy/internal/proxy"
	"paepcke.de/goproxy/internal/stats"
	"paepcke.de/goproxy/internal/store"
	"paepcke.de/goproxy/internal/sumdb"
	"paepcke.de/goproxy/internal/systemd"
	"paepcke.de/goproxy/internal/ui"
	"paepcke.de/goproxy/internal/version"
)

func main() {
	addr := flag.String("addr", "", "listen address (default :5100, env BIND_ADDR; systemd socket activation takes precedence)")
	root := flag.String("root", "", "module cache directory (default cache, env CACHE_ROOT)")
	upstream := flag.String("upstream", "https://proxy.golang.org", "upstream module proxy")
	sumdbURL := flag.String("sumdb", "https://sum.golang.org", "checksum database base url")
	sumdbKey := flag.String("sumdb-key", sumdb.DefaultKey, "checksum database public key")
	verify := flag.Bool("verify", true, "verify module hashes against the checksum database")
	flag.Parse()

	debug := envBool("DEBUG")
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel(debug)}))
	ver := version.Get()
	listenAddr := resolveAddr(*addr)

	st, err := store.New(resolveRoot(*root))
	if err != nil {
		log.Error("startup", "err", err)
		os.Exit(1)
	}
	db, err := sumdb.New(*sumdbURL, *sumdbKey, filepath.Join(st.Root(), "sumdb"), sumdbClient(), log)
	if err != nil {
		log.Error("startup", "err", err)
		os.Exit(1)
	}

	dbPath := resolveStatsDB(st.Root(), defaultStateDir)

	// Long-term stats: cgo-free SQLite, everything survives reboots.
	hist, err := history.Open(dbPath)
	if err != nil {
		log.Error("startup", "err", err)
		os.Exit(1)
	}
	if imported, err := hist.ImportLegacyJSON(filepath.Join(filepath.Dir(dbPath), "stats.json")); err != nil {
		log.Warn("importing legacy stats snapshot", "err", err)
	} else if imported {
		log.Info("migrated stats.json into stats.db", "file", filepath.Join(filepath.Dir(dbPath), "stats.json.imported"))
	}
	totals, err := hist.LoadTotals()
	if err != nil {
		log.Error("loading stats", "err", err)
		os.Exit(1)
	}
	feed, err := hist.RecentEvents(512)
	if err != nil {
		log.Error("loading stats", "err", err)
		os.Exit(1)
	}
	usage, err := hist.LoadModuleUsage()
	if err != nil {
		log.Error("loading module usage", "err", err)
		os.Exit(1)
	}
	st.RestoreUsage(usage)
	clientUsage, err := hist.LoadClients(clientLoadLimit)
	if err != nil {
		log.Warn("loading client usage", "err", err)
	}
	tracker := clients.New()
	tracker.Restore(clientUsage)
	m := stats.New(hist, stats.WithStoreSampler(func() stats.StoreStat {
		su := st.Summary()
		return stats.StoreStat{
			Bytes:    su.Size,
			Modules:  int64(su.Modules),
			Versions: int64(su.Versions),
			Files:    int64(su.Files),
		}
	}))
	m.Seed(totals, feed)
	neighbours := envList("NEIGHBOUR_CACHES")
	p, err := proxy.New(st, db, m, *upstream, neighbours, *verify, moduleClient(), log)
	if err != nil {
		log.Error("startup", "err", err)
		os.Exit(1)
	}
	// The neighbour fleet is probed at startup and then every 10 minutes,
	// so the dashboard shows which peers are currently reachable.
	peers := fleet.New(neighbours, moduleClient(), log)
	// The warmer pre-caches the hottest modules; the dashboard shows the
	// details of the most recent pass.
	warmer := proxy.NewWarmer(p, st.Popular, log)
	uiH := ui.New(m, st, hist, tracker, ver, peers, warmer)

	// Socket activation: serve on the sockets systemd passed (LISTEN_FDS)
	// when present; otherwise bind the resolved address. EXIT_ON_IDLE is
	// honored in this mode because systemd keeps the socket and restarts
	// the process on demand.
	listeners, err := systemd.Listeners()
	if err != nil {
		log.Error("startup", "err", err)
		os.Exit(1)
	}
	socketActivated := len(listeners) > 0
	declaredActivation := envBool("SOCKET_ACTIVATION")
	if declaredActivation && !socketActivated {
		log.Warn("SOCKET_ACTIVATION=1 but no systemd sockets passed; binding", "addr", listenAddr)
	}
	exitOnIdle := envBool("EXIT_ON_IDLE") && (socketActivated || declaredActivation)
	if envBool("EXIT_ON_IDLE") && !exitOnIdle {
		log.Warn("EXIT_ON_IDLE=1 ignored: not socket-activated")
	}

	var track *connTracker
	if exitOnIdle {
		track = &connTracker{}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Background maintenance: neighbour health, checksum-database freshness
	// and the hourly fleet cache sync. All of it stops with ctx.
	indexed := make(chan struct{})
	var jobs sync.WaitGroup
	jobs.Add(4)
	go func() {
		defer jobs.Done()
		peers.Run(ctx)
	}()
	go func() {
		defer jobs.Done()
		db.RefreshLoop(ctx, sumdbRefreshInterval)
	}()
	go func() {
		defer jobs.Done()
		runMaintenance(ctx, hist, m, log, ver)
	}()
	go func() {
		defer jobs.Done()
		// Wait for the startup index scan: the ranking of the hottest
		// modules comes from the cache index.
		select {
		case <-ctx.Done():
			return
		case <-indexed:
		}
		warmer.Run(ctx)
	}()

	go func() {
		defer close(indexed)
		start := time.Now()
		n, err := st.Scan(ctx)
		if err != nil {
			log.Error("scanning cache", "err", err)
			return
		}
		m.Indexed(n)
		log.Info("cache index rebuilt", "modules", n, "took", time.Since(start).Round(time.Millisecond))
	}()
	histDone := make(chan struct{})
	go func() {
		hist.Run(ctx) // daily event-log pruning + WAL hygiene
		close(histDone)
	}()
	statsDone := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(statsDone)
	}()
	usageDone := make(chan struct{})
	go func() {
		defer close(usageDone)
		tick := time.NewTicker(moduleFlushInterval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				persistUsage(hist, st, tracker, log)
			}
		}
	}()

	handler := dispatch(p, uiH, ver)
	handler = withClientStats(handler, tracker)
	if debug {
		handler = withAccessLog(handler, log)
	}
	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	if track != nil {
		srv.ConnState = track.state
	}
	serveDesc := describeListeners(listenAddr, listeners)
	m.Started(ver, serveDesc)

	errCh := make(chan error, 1+len(listeners))
	if socketActivated {
		for _, l := range listeners {
			go func(l net.Listener) {
				errCh <- srv.Serve(l)
			}(l)
		}
	} else {
		go func() {
			errCh <- srv.ListenAndServe()
		}()
	}

	var idleExit <-chan struct{}
	if exitOnIdle {
		idleExit = watchIdle(ctx, track, idleGrace)
	}

	log.Info("listening", "addr", serveDesc, "socket-activated", socketActivated,
		"exit-on-idle", exitOnIdle, "version", ver, "root", st.Root(), "stats-db", dbPath,
		"upstream", *upstream, "neighbours", neighbours, "sumdb", *sumdbURL, "verify", *verify)

	exitCode := 0
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("server", "err", err)
			exitCode = 1
		}
	case <-ctx.Done():
	case <-idleExit:
		log.Info("idle exit: no in-flight requests for " + idleGrace.String() +
			" (systemd restarts on the next connection)")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown", "err", err)
		exitCode = 1
	}
	stop()
	// Let the background jobs observe the cancellation before the stats
	// collector takes its final snapshot, so prewarmed bandwidth is
	// included in the last bucket.
	jobs.Wait()
	// Wait for the collector to flush its final bucket, then close the
	// stats database cleanly.
	<-statsDone
	<-histDone
	<-usageDone
	// Final module and client usage snapshot before the database closes, so
	// a restart resumes the served-bandwidth totals where they left off.
	persistUsage(hist, st, tracker, log)
	if err := hist.Close(); err != nil {
		log.Error("closing stats database", "err", err)
		exitCode = 1
	}
	log.Info("stopped")
	if exitCode != 0 {
		// Preserve the failure for systemd after the final stats flush
		// and database checkpoint instead of exiting from the select.
		os.Exit(exitCode)
	}
}

// moduleFlushInterval bounds how much per-module and per-client access
// accounting a crash can lose; both are always written once more on
// shutdown.
const moduleFlushInterval = 30 * time.Second

// clientLoadLimit bounds how many persisted clients are restored into the
// in-memory ranking at startup.
const clientLoadLimit = clients.MaxTracked

// sumdbRefreshInterval keeps the signed checksum-database tree head fresh,
// so verification never starts from a stale transparency-log view.
const sumdbRefreshInterval = 15 * time.Minute

// persistUsage writes the modules and clients that saw traffic since the
// last flush, so the dashboard rankings survive restarts.
func persistUsage(hist *history.DB, st *store.Store, tracker *clients.Tracker, log *slog.Logger) {
	if err := hist.SaveModuleUsage(st.TakeUsage()); err != nil {
		log.Warn("persisting module usage", "err", err)
	}
	if err := hist.SaveClients(tracker.TakeDirty()); err != nil {
		log.Warn("persisting client usage", "err", err)
	}
}

// resolveAddr picks the self-bind address: an explicit -addr flag wins,
// then the BIND_ADDR environment variable, then the default.
func resolveAddr(flagAddr string) string {
	if flagAddr != "" {
		return flagAddr
	}
	if env := os.Getenv("BIND_ADDR"); env != "" {
		return env
	}
	return ":5100"
}

// resolveRoot picks the module cache directory: an explicit -root flag
// wins, then the CACHE_ROOT environment variable, then "cache". CACHE_ROOT
// lets deployments put the (potentially huge, wipeable) module storage on a
// dedicated path or disk without touching the command line.
func resolveRoot(flagRoot string) string {
	if flagRoot != "" {
		return flagRoot
	}
	if env := os.Getenv("CACHE_ROOT"); env != "" {
		return env
	}
	return "cache"
}

// defaultStateDir is the fixed home of the long-term stats database:
// deployments create it via the systemd StateDirectory directive.
const defaultStateDir = "/var/lib/goproxy"

// resolveStatsDB picks the SQLite stats database location: the default
// state directory (/var/lib/goproxy, normally provided by systemd) when it
// exists, otherwise <root>/stats.db next to the module cache for
// unprivileged local runs. The location is fixed policy, not configuration:
// wiping the cache never wipes the history.
func resolveStatsDB(root, stateDir string) string {
	if fi, err := os.Stat(stateDir); err == nil && fi.IsDir() {
		return filepath.Join(stateDir, "stats.db")
	}
	return filepath.Join(root, "stats.db")
}

// logLevel keeps per-request access and failure lines (Debug) out of the
// journal unless DEBUG=1; startup state, warnings and security refusals
// are always logged.
func logLevel(debug bool) slog.Level {
	if debug {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

// envList splits an environment variable into a list. Commas and
// whitespace both separate entries, so NEIGHBOUR_CACHES accepts
// "http://a:9090,http://b:9090" as well as space- or newline separated
// values.
func envList(name string) []string {
	return strings.Fields(strings.ReplaceAll(os.Getenv(name), ",", " "))
}

// envBool reports whether an environment variable holds a truthy value.
func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// describeListeners renders what the server serves: systemd-passed
// sockets when socket-activated, otherwise the bound address.
func describeListeners(addr string, listeners []net.Listener) string {
	if len(listeners) == 0 {
		return addr
	}
	addrs := make([]string, len(listeners))
	for i, l := range listeners {
		addrs[i] = l.Addr().String()
	}
	return "systemd (" + strings.Join(addrs, ", ") + ")"
}

// dispatch routes dashboard traffic to the ui and everything else to the
// module protocol handler. Module paths are arbitrary, so the proxy owns
// the fallback route.
func dispatch(p *proxy.Proxy, uiH *ui.Handler, ver string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/" || path == "/index.html" || path == "/api/dashboard" ||
			path == "/api/history" || path == "/api/events" ||
			strings.HasPrefix(path, "/static/") || path == "/favicon.ico":
			uiH.ServeHTTP(w, r)
		case path == "/healthz":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintf(w, "ok %s\n", ver)
		case strings.HasPrefix(path, "/sumdb/"):
			p.ServeSumDB(w, r)
		default:
			p.ServeModule(w, r)
		}
	})
}

// moduleClient fetches large zips without a total timeout, but bounds
// dialing, TLS and header waits.
func moduleClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   16,
			ForceAttemptHTTP2:     true,
		},
	}
}

func sumdbClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}
