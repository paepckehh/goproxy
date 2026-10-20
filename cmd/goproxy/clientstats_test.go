package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"paepcke.de/goproxy/internal/clients"
)

func TestClientID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, xff, remote, want string
	}{
		{"forwarded", "203.0.113.7, 10.0.0.1", "127.0.0.1:1234", "203.0.113.7"},
		{"remote", "", "198.51.100.9:4321", "198.51.100.9"},
		{"ipv6", "", "[2001:db8::1]:443", "2001:db8::1"},
		{"forwarded ipv4 mapped", "::ffff:192.0.2.7", "127.0.0.1:1", "192.0.2.7"},
		{"unusable", "not-an-ip", "garbage", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, "/example.com/m/@v/v1.0.0.mod", nil)
			r.RemoteAddr = tc.remote
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := clientID(r); got != tc.want {
				t.Errorf("clientID = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWithClientStats(t *testing.T) {
	t.Parallel()
	tracker := clients.New()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
	})
	h := withClientStats(next, tracker)

	req := httptest.NewRequest(http.MethodGet, "/example.com/m/@v/v1.0.0.mod", nil)
	req.RemoteAddr = "198.51.100.9:1234"
	h.ServeHTTP(httptest.NewRecorder(), req)

	top := tracker.TopByRequests(1)
	if len(top) != 1 || top[0].ID != "198.51.100.9" || top[0].Requests != 1 || top[0].BytesDown != 5 {
		t.Fatalf("ranking = %+v, want one request with 5 bytes from the peer", top)
	}

	// Dashboard and health traffic never pollutes the client ranking.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/dashboard", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if n := tracker.Count(); n != 1 {
		t.Errorf("tracked = %d, want only the module client", n)
	}
}
