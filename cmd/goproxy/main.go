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
	"syscall"
	"time"

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
	root := flag.String("root", "cache", "cache directory")
	upstream := flag.String("upstream", "https://proxy.golang.org", "upstream module proxy")
	sumdbURL := flag.String("sumdb", "https://sum.golang.org", "checksum database base url")
	sumdbKey := flag.String("sumdb-key", sumdb.DefaultKey, "checksum database public key")
	verify := flag.Bool("verify", true, "verify module hashes against the checksum database")
	statsDB := flag.String("stats-db", "", "absolute path of the long-term stats database (default <root>/stats.db, env STATS_DB)")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ver := version.Get()
	listenAddr := resolveAddr(*addr)

	st, err := store.New(*root)
	if err != nil {
		log.Error("startup", "err", err)
		os.Exit(1)
	}
	db, err := sumdb.New(*sumdbURL, *sumdbKey, filepath.Join(st.Root(), "sumdb"), sumdbClient(), log)
	if err != nil {
		log.Error("startup", "err", err)
		os.Exit(1)
	}

	dbPath, err := resolveStatsDB(*statsDB, st.Root())
	if err != nil {
		log.Error("startup", "err", err)
		os.Exit(1)
	}

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
	p, err := proxy.New(st, db, m, *upstream, *verify, moduleClient(), log)
	if err != nil {
		log.Error("startup", "err", err)
		os.Exit(1)
	}
	uiH := ui.New(m, st, hist, ver)

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

	go func() {
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

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           dispatch(p, uiH, ver),
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
		"upstream", *upstream, "sumdb", *sumdbURL, "verify", *verify)

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("server", "err", err)
			os.Exit(1)
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
	}
	stop()
	// Wait for the collector to flush its final bucket, then close the
	// stats database cleanly.
	<-statsDone
	<-histDone
	if err := hist.Close(); err != nil {
		log.Error("closing stats database", "err", err)
	}
	log.Info("stopped")
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

// resolveStatsDB picks the SQLite stats database location: an explicit
// -stats-db flag wins, then the STATS_DB environment variable, then
// <root>/stats.db. An explicit location must be absolute so deployments
// can keep the database in e.g. /var/lib/goproxy, separate from the
// potentially huge and wipeable module cache.
func resolveStatsDB(flagPath, root string) (string, error) {
	path := flagPath
	if path == "" {
		path = os.Getenv("STATS_DB")
	}
	if path == "" {
		return filepath.Join(root, "stats.db"), nil
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("stats database path %q must be absolute", path)
	}
	return path, nil
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
			path == "/api/history" ||
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
