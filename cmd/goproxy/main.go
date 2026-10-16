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

	"paepcke.de/goproxy/internal/proxy"
	"paepcke.de/goproxy/internal/stats"
	"paepcke.de/goproxy/internal/store"
	"paepcke.de/goproxy/internal/sumdb"
	"paepcke.de/goproxy/internal/ui"
	"paepcke.de/goproxy/internal/version"
)

func main() {
	addr := flag.String("addr", ":5100", "listen address")
	root := flag.String("root", "cache", "cache directory")
	upstream := flag.String("upstream", "https://proxy.golang.org", "upstream module proxy")
	sumdbURL := flag.String("sumdb", "https://sum.golang.org", "checksum database base url")
	sumdbKey := flag.String("sumdb-key", sumdb.DefaultKey, "checksum database public key")
	verify := flag.Bool("verify", true, "verify module hashes against the checksum database")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ver := version.Get()

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
	m := stats.New(filepath.Join(st.Root(), "stats.json"))
	if err := m.Load(); err != nil {
		log.Warn("loading stats snapshot", "err", err)
	}
	p, err := proxy.New(st, db, m, *upstream, *verify, moduleClient(), log)
	if err != nil {
		log.Error("startup", "err", err)
		os.Exit(1)
	}
	uiH := ui.New(m, st, ver)

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
	go m.Run(ctx)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           dispatch(p, uiH, ver),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	m.Started(ver, *addr)

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", *addr, "version", ver, "root", st.Root(),
			"upstream", *upstream, "sumdb", *sumdbURL, "verify", *verify)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("server", "err", err)
			os.Exit(1)
		}
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown", "err", err)
	}
	stop()
	if err := m.Save(); err != nil {
		log.Error("saving stats", "err", err)
	}
	log.Info("stopped")
}

// dispatch routes dashboard traffic to the ui and everything else to the
// module protocol handler. Module paths are arbitrary, so the proxy owns
// the fallback route.
func dispatch(p *proxy.Proxy, uiH *ui.Handler, ver string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/" || path == "/index.html" || path == "/api/dashboard" ||
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
