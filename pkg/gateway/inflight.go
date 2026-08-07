// ConnState-based in-flight request counter (Tier A8 / ADR-083
// / code-review fix #5).
//
// The DNSHandoff orchestrator waits for in-flight requests to
// reach zero before flipping DNS. Without a real counter,
// pkg/gateway/dns_handoff.go's noopInFlight returned 0
// unconditionally — the drain finished in <50 ms regardless of
// how many HTTP requests were actually in flight on the dying
// leader, racing the new leader's DNS flip against requests
// still hitting the old IP.
//
// ConnStateTracker is the production surface. Wire it via
// http.Server.ConnState (set in cmd/gatewayd-public/main.go's
// buildServers). The orchestrator hands the same tracker to
// DNSHandoff.InFlight — one tracker per listener, one Count()
// accessor per drain.
//
// Thread-safety: atomic.Int64 so SetStandbyState in
// pkg/wire/metrics.go can read .Count() without a mutex. ConnState
// is invoked by net/http on a connection-tracking goroutine; the
// caller does not hold a lock.

package gateway

import (
	"net"
	"net/http"
	"sync/atomic"
)

// ConnStateTracker counts open HTTP connections via the
// http.Server.ConnState callback. Implements InFlightCounter.
//
// Transitions counted:
//
//	StateNew       → +1  (connection accepted)
//	StateClosed    → -1  (connection fully closed)
//	StateHijacked  → -1  (h2c upgrade / WebSocket — connection
//	                       left the HTTP layer)
//	StateActive    → no change (idle on the keep-alive pool)
type ConnStateTracker struct {
	n atomic.Int64
}

// NewConnStateTracker constructs a zero-valued tracker. Use the
// returned pointer on http.Server.ConnState AND pass the same
// pointer to DNSHandoff.InFlight.
func NewConnStateTracker() *ConnStateTracker {
	return &ConnStateTracker{}
}

// ConnState is the http.Server.ConnState callback shape. Wire
// via srv.ConnState = tracker.ConnState.
func (c *ConnStateTracker) ConnState(_ net.Conn, state http.ConnState) {
	switch state {
	case http.StateNew:
		c.n.Add(1)
	case http.StateClosed, http.StateHijacked:
		c.n.Add(-1)
	}
}

// Count returns the current in-flight count. Implements
// InFlightCounter. Safe to call from any goroutine.
func (c *ConnStateTracker) Count() int {
	return int(c.n.Load())
}
