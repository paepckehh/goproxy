// Package fleet probes the neighbour caches a proxy is chained with, so the
// dashboard can show whether each peer is reachable and which version it
// runs. The probe is a plain GET of the peer's /healthz endpoint; it never
// touches module data.
package fleet

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// ProbeInterval is how often a peer is re-evaluated.
	ProbeInterval = 10 * time.Minute
	// probeTimeout bounds one peer probe, so an unresponsive neighbour
	// cannot stall the sweep.
	probeTimeout = 5 * time.Second
	healthPath   = "healthz"
)

// Status is the last observed health of one neighbour cache.
type Status struct {
	URL       string    `json:"url"`
	Online    bool      `json:"online"`
	Version   string    `json:"version,omitempty"`
	LatencyMS int64     `json:"latencyMs"`
	CheckedAt time.Time `json:"checkedAt"`
	Err       string    `json:"err,omitempty"`
}

// Monitor probes the configured neighbour caches at startup and then every
// interval. It is safe for concurrent use: the proxy keeps serving while a
// sweep updates the reported statuses.
type Monitor struct {
	peers    []*url.URL
	client   *http.Client
	log      *slog.Logger
	interval time.Duration

	mu     sync.RWMutex
	status []Status
}

// New creates a monitor for the given neighbour base URLs. Entries that are
// not http(s) URLs with a host are skipped: the proxy rejects such a
// configuration at startup, so this only guards the standalone probe.
func New(neighbours []string, client *http.Client, log *slog.Logger) *Monitor {
	if log == nil {
		log = slog.Default()
	}
	if client == nil {
		client = &http.Client{}
	}
	// The probe needs a total deadline, unlike module fetches where a
	// large zip streams without one.
	c := *client
	c.Timeout = probeTimeout
	m := &Monitor{client: &c, log: log, interval: ProbeInterval}
	seen := make(map[string]struct{}, len(neighbours))
	for _, raw := range neighbours {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			continue
		}
		key := strings.TrimRight(u.String(), "/")
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		m.peers = append(m.peers, u)
	}
	return m
}

// Statuses returns a snapshot of the last sweep, in configuration order.
func (m *Monitor) Statuses() []Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Status, len(m.status))
	copy(out, m.status)
	return out
}

// Probe sweeps every neighbour in parallel and replaces the reported
// statuses with the result.
func (m *Monitor) Probe(ctx context.Context) {
	if len(m.peers) == 0 {
		return
	}
	out := make([]Status, len(m.peers))
	var wg sync.WaitGroup
	for i, peer := range m.peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = m.probe(ctx, peer)
		}()
	}
	wg.Wait()
	m.mu.Lock()
	m.status = out
	m.mu.Unlock()
}

// Run probes immediately, then once per interval until ctx is cancelled.
func (m *Monitor) Run(ctx context.Context) {
	if len(m.peers) == 0 {
		return
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		m.Probe(ctx)
		timer.Reset(m.interval)
	}
}

func (m *Monitor) probe(ctx context.Context, peer *url.URL) Status {
	// Redacted keeps credentials in the configured URL out of the
	// dashboard payload and the journal.
	st := Status{URL: strings.TrimRight(peer.Redacted(), "/"), CheckedAt: time.Now().UTC()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, peer.JoinPath(healthPath).String(), nil)
	if err != nil {
		st.Err = err.Error()
		return st
	}
	start := time.Now()
	resp, err := m.client.Do(req)
	st.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		st.Err = err.Error()
		m.log.Debug("fleet: neighbour unreachable", "neighbour", peer.Host, "err", err)
		return st
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	if resp.StatusCode != http.StatusOK {
		st.Err = fmt.Sprintf("healthz status %d", resp.StatusCode)
		m.log.Debug("fleet: neighbour unhealthy", "neighbour", peer.Host, "status", resp.StatusCode)
		return st
	}
	st.Online = true
	st.Version = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(body)), "ok"))
	return st
}
