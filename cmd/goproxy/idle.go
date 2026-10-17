package main

import (
	"context"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// idleGrace is how long the process must see no in-flight request before
// EXIT_ON_IDLE shuts it down. systemd keeps the listening socket open and
// restarts the process on the next connection, so exiting while idle is
// safe and frees the cache-owner process between builds.
const idleGrace = 30 * time.Second

// connTracker counts connections with an in-flight request, so
// EXIT_ON_IDLE can tell an idle server from a busy one. Idle keep-alive
// connections do not count as busy: only open transfers do. It implements
// http.Server.ConnState, which reports every state change of a connection
// exactly once, in order.
type connTracker struct {
	states sync.Map // net.Conn -> bool: serving a request right now
	busy   atomic.Int64
}

func (t *connTracker) state(c net.Conn, s http.ConnState) {
	busy := s == http.StateActive || s == http.StateHijacked
	if prev, ok := t.states.Swap(c, busy); ok {
		was, _ := prev.(bool)
		switch {
		case busy && !was:
			t.busy.Add(1)
		case !busy && was:
			t.busy.Add(-1)
		}
	} else if busy {
		t.busy.Add(1)
	}
	if s == http.StateClosed {
		t.states.Delete(c)
	}
}

// watchIdle closes the returned channel once no request has been in
// flight for the whole grace period. Cancelling ctx stops the watch.
func watchIdle(ctx context.Context, t *connTracker, grace time.Duration) <-chan struct{} {
	idle := make(chan struct{})
	go func() {
		defer close(idle)
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		lastBusy := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if t.busy.Load() > 0 {
					lastBusy = time.Now()
				} else if time.Since(lastBusy) >= grace {
					return
				}
			}
		}
	}()
	return idle
}
