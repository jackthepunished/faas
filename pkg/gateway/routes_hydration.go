// Package gateway — routes_hydration.go owns the "is the route cache
// loaded yet?" signal the post-Tier-A7 /readyz handler reads.
//
// Background: pkg/gateway/routes.go::RouteCache is a passive LRU;
// today's gatewayd fills it lazily — the first request for a host
// triggers a Postgres lookup, subsequent requests hit the cache. In
// one-box that was fine: the first request for a cold app paid the
// lookup cost and the wake gate cold-started the VM in parallel.
//
// After the Tier A7 edge split (ADR-070), gatewayd-internal must NOT
// forward traffic until the cache is at least *primed* — otherwise
// the LB would route traffic to an instance that doesn't yet know
// what host maps to what app, and every cold request would double-
// pay the lookup latency (once at the public daemon, once at the
// internal daemon).
//
// The split triggers a re-shape:
//
//  1. At boot, gatewayd-internal calls LoadFromPG() (the existing
//     helper at cmd/gatewayd/backend.go:loadRouteCache) which runs
//     one `SELECT host, app_id FROM …` and Pushes every row into
//     the cache.
//  2. While that SELECT is running, the daemon is NOT ready — the
//     hydration bit is false.
//  3. On completion, the daemon calls MarkHydrated(), which flips
//     the bit to true.
//  4. If the SELECT fails (Postgres down at boot), the bit stays
//     false and /readyz returns 503 — the daemon refuses to
//     forward traffic until it can read the route table.
//  5. A subsequent Reset() — fired by app_changed / domain_changed
//     pg_notify — keeps the hydration bit at its current value
//     (pg_notify delivery implies the route table changed but is
//     still readable; the daemon doesn't need to re-mark).
//  6. The ONLY signal that flips hydration from true→false is a
//     Postgres-readiness drop; if PG goes away mid-flight, the
//     daemon's /readyz returns 503 (handled by the PG-ping signal
//     in pkg/gateway/readiness.go — separate concern).
//
// The hydration bit lives in its own small struct (rather than a
// field on RouteCache) so the cache itself stays a pure LRU with no
// readiness semantics — same separation pkg/sched/warmhint_cache.go
// uses (the cache is pure, the broadcaster tracks state).
package gateway

import (
	"context"
	"sync"
	"sync/atomic"
)

// RouteCacheHydration tracks whether a RouteCache has been seeded
// from Postgres at least once. Safe for concurrent use; the bit is
// atomic and the reason is mutex-guarded so the /readyz scrape can
// read both without taking the cache's mutex.
//
// The zero value is "not hydrated"; NewRouteCacheHydration is a
// no-op constructor for parity with the rest of pkg/gateway's
// "constructor returns the same thing as the zero value" pattern.
type RouteCacheHydration struct {
	hydrated atomic.Bool
	mu       sync.RWMutex
	reason   string
}

// NewRouteCacheHydration returns a fresh hydration tracker in the
// "not hydrated" state. Callers wire it next to the *RouteCache
// they own; the cache itself is unaware of the tracker.
func NewRouteCacheHydration() *RouteCacheHydration {
	return &RouteCacheHydration{}
}

// MarkHydrated flips the bit to true with an empty reason. Idempotent
// — calling twice is harmless. This is the bit the post-split daemon
// flips after LoadFromPG returns successfully.
func (h *RouteCacheHydration) MarkHydrated() {
	h.hydrated.Store(true)
	h.mu.Lock()
	h.reason = ""
	h.mu.Unlock()
}

// MarkUnhydrated flips the bit to false with a reason. The reason
// surfaces in /readyz's body so operators see why a daemon is
// still scraping the route table. Called from LoadFromPG's error
// path (Postgres unreachable at boot) and from the daemon's drain
// sequence (so /readyz flips 503 the moment SIGTERM lands).
func (h *RouteCacheHydration) MarkUnhydrated(reason string) {
	h.hydrated.Store(false)
	h.mu.Lock()
	h.reason = reason
	h.mu.Unlock()
}

// Hydrated returns the current bit and the last reason. The /readyz
// handler calls this; daemons wire it into a ReadyzProbe.
func (h *RouteCacheHydration) Hydrated() (bool, string) {
	return h.hydrated.Load(), h.lastReason()
}

func (h *RouteCacheHydration) lastReason() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.reason
}

// RouteCacheLoader is the seam the daemon wires to fill a RouteCache
// at boot. It exists so the gateway package can ship a tiny test
// fake (cmd/gatewayd-internal/backend_test.go) without dragging the
// real Postgres-backed loader into unit tests.
//
// The contract:
//   - On success, the loader has Put'd every (host, appID) pair into
//     `cache` AND called hydration.MarkHydrated().
//   - On failure, the loader has NOT called MarkHydrated (the bit
//     stays false) AND has called hydration.MarkUnhydrated(reason)
//     with the error message so /readyz surfaces it.
//   - On a context-cancelled shutdown, the loader is allowed to
//     short-circuit before fully populating the cache; the hydration
//     bit stays false and the daemon exits.
//
// Production wiring: cmd/gatewayd-internal/backend.go::LoadRouteCache
// implements RouteCacheLoader against *state.PgStore.
type RouteCacheLoader interface {
	LoadRouteCache(ctx context.Context, cache *RouteCache, hydration *RouteCacheHydration) error
}
