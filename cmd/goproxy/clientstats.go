package main

import (
	"net"
	"net/http"
	"net/netip"
	"strings"

	"paepcke.de/goproxy/internal/clients"
)

// withClientStats accounts every module/sumdb response against the
// requesting client. Dashboard, health and static traffic is excluded, so
// the ranking reflects builds rather than monitoring.
func withClientStats(next http.Handler, tracker *clients.Tracker) http.Handler {
	if tracker == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !clientTraffic(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		// Reuse the access-log writer: it counts the body and keeps the
		// optional Flusher/ReaderFrom behavior of the wrapped writer.
		cw := &accessLogWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(cw, r)
		tracker.Record(clientID(r), cw.n)
	})
}

// clientTraffic reports whether a path carries module traffic worth
// attributing to a client.
func clientTraffic(path string) bool {
	switch path {
	case "/", "/index.html", "/favicon.ico", "/healthz",
		"/api/dashboard", "/api/history", "/api/events":
		return false
	}
	return !strings.HasPrefix(path, "/static/")
}

// clientID derives a stable client identity. The reverse proxy in front of
// the deployment overwrites X-Forwarded-For with the real peer, so its
// first entry is the client; without one the socket address is used. The
// header only feeds statistics, never an access decision.
func clientID(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		if addr, err := netip.ParseAddr(strings.TrimSpace(first)); err == nil {
			return addr.Unmap().String()
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return ""
	}
	return addr.Unmap().String()
}
