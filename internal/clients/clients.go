// Package clients tracks per-client proxy usage: request counts,
// downstream bytes served and the last time a client appeared.
//
// The counters live in memory and the dashboard reads them there; they are
// persisted by the stats recorder flush loop so a restart keeps the
// long-term ranking. The in-memory table is bounded: a client map that grew
// without limit would be a memory amplifier under a scan of many addresses.
package clients

import (
	"cmp"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// MaxTracked bounds the in-memory client table. When it is full, a new
// client replaces an arbitrary tracked one, so a burst of fresh addresses
// can never grow the process without bound.
const MaxTracked = 4096

// Usage is the counter set of one client. Absolute values are persisted
// and restored, so the ranking survives restarts.
type Usage struct {
	Requests  int64     `json:"requests"`
	BytesDown int64     `json:"bytesDown"`
	LastSeen  time.Time `json:"lastSeen"`
}

// Stat is one ranked client for the dashboard.
type Stat struct {
	ID string `json:"id"`
	Usage
}

type counter struct {
	requests  atomic.Int64
	bytesDown atomic.Int64
	lastSeen  atomic.Int64 // unix nanoseconds
	dirty     atomic.Bool
}

func (c *counter) record(down int64) {
	c.requests.Add(1)
	c.bytesDown.Add(down)
	c.lastSeen.Store(time.Now().UnixNano())
	c.dirty.Store(true)
}

func (c *counter) snapshot() Usage {
	u := Usage{Requests: c.requests.Load(), BytesDown: c.bytesDown.Load()}
	if n := c.lastSeen.Load(); n != 0 {
		u.LastSeen = time.Unix(0, n).UTC()
	}
	return u
}

// Tracker is the concurrency-safe in-memory client table.
type Tracker struct {
	mu      sync.RWMutex
	clients map[string]*counter
}

// New returns an empty tracker.
func New() *Tracker {
	return &Tracker{clients: make(map[string]*counter)}
}

// Record accounts one request and the bytes served downstream for id. An
// empty id (no usable address) is ignored rather than collapsed into one
// anonymous row.
func (t *Tracker) Record(id string, down int64) {
	if id == "" {
		return
	}
	t.mu.RLock()
	c := t.clients[id]
	t.mu.RUnlock()
	if c == nil {
		t.mu.Lock()
		if c = t.clients[id]; c == nil {
			if len(t.clients) >= MaxTracked {
				for k := range t.clients {
					delete(t.clients, k)
					break
				}
			}
			c = &counter{}
			t.clients[id] = c
		}
		t.mu.Unlock()
	}
	c.record(down)
}

// Restore seeds the counters from persistent storage before new traffic is
// recorded. Counters recorded later add to the restored totals.
func (t *Tracker) Restore(usage map[string]Usage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, u := range usage {
		if id == "" {
			continue
		}
		c := &counter{}
		c.requests.Store(u.Requests)
		c.bytesDown.Store(u.BytesDown)
		if !u.LastSeen.IsZero() {
			c.lastSeen.Store(u.LastSeen.UnixNano())
		}
		t.clients[id] = c
	}
}

// TakeDirty returns the clients whose counters changed since the last call
// and marks them clean, so the persistence loop only writes active clients.
func (t *Tracker) TakeDirty() map[string]Usage {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]Usage)
	for id, c := range t.clients {
		if c.dirty.Swap(false) {
			out[id] = c.snapshot()
		}
	}
	return out
}

// Count reports how many clients are tracked in memory.
func (t *Tracker) Count() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.clients)
}

// TopByRequests returns up to n clients ordered by requests, then bytes
// served, then id. A negative n returns every tracked client.
func (t *Tracker) TopByRequests(n int) []Stat {
	return t.top(n, func(a, b Stat) int {
		if c := cmp.Compare(b.Requests, a.Requests); c != 0 {
			return c
		}
		return cmp.Compare(b.BytesDown, a.BytesDown)
	})
}

// TopByBytes returns up to n clients ordered by downstream bytes served.
func (t *Tracker) TopByBytes(n int) []Stat {
	return t.top(n, func(a, b Stat) int {
		if c := cmp.Compare(b.BytesDown, a.BytesDown); c != 0 {
			return c
		}
		return cmp.Compare(b.Requests, a.Requests)
	})
}

func (t *Tracker) top(n int, order func(a, b Stat) int) []Stat {
	if n == 0 {
		return nil
	}
	t.mu.RLock()
	out := make([]Stat, 0, len(t.clients))
	for id, c := range t.clients {
		out = append(out, Stat{ID: id, Usage: c.snapshot()})
	}
	t.mu.RUnlock()
	slices.SortFunc(out, func(a, b Stat) int {
		if c := order(a, b); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}
