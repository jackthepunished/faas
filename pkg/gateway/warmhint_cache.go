// warmhint_cache.go — gatewayd's sticky-warm affinity cache
// (ADR-025 axis 4).
//
// The picker (pkg/gateway/pgbackend.go::PGBackend.Pick) reads
// WarmHintFunc on every request to bias traffic toward the
// compute_node that last warmed the app. The hint is sourced
// from schedd's StreamWarmHints gRPC stream — see
// pkg/sched/warmhint.go for the producer side.
//
// Disconnect policy (Phase 3 review): freeze. A 5-second blip
// costs zero picker behaviour change. A 30-minute outage means
// stale hints, but the picker is bias-only and falls through to
// per-node healthyCount scoring on saturation (ADR-009). The
// WarmAffinityTTL on the schedd side (api.WarmAffinityTTL,
// default 30 min) bounds staleness on both sides simultaneously,
// so a gatewayd that reconnects after a long outage converges
// within one TTL as new emits land.
//
// Initial state on gatewayd startup is empty — Pick degrades to
// least-loaded via the existing healthyCount path (ADR-005 cold
// boot must always work). The cache is process-local; a second
// gatewayd sees the same hint stream and converges independently.
//
// No TTL on the cache itself: the stream IS the source of truth.
// WarmAffinityTTL on the schedd side governs eviction of old
// entries; if schedd forgets an entry it stops emitting for that
// app, and the gatewayd's hint lingers harmlessly until a future
// emit replaces it.

package gateway

import (
	"sync"
)

// WarmHintCache is the in-memory (appID → nodeID) map the picker
// reads via HintFunc. Safe for concurrent use: Hint takes the
// read lock, Update/Forget take the write lock. The map is
// unbounded; its cardinality is bounded by Σ(hot_apps), not by
// fleet size, so memory cost is small.
//
// Exported because cmd/gatewayd constructs it (the stream
// consumer holds a pointer) and feeds it from
// cmd/gatewayd/warmhints.go.
type WarmHintCache struct {
	mu sync.RWMutex
	m  map[string]string // appID -> nodeID
}

// NewWarmHintCache returns an empty cache ready for
// Update/Forget calls. HintFunc adapts the cache to the
// picker's WarmHintFunc signature (pkg/gateway/pgbackend.go:26).
func NewWarmHintCache() *WarmHintCache {
	return &WarmHintCache{m: make(map[string]string)}
}

// Update stamps (appID → nodeID). Called from the stream
// consumer in cmd/gatewayd/warmhints.go on every WarmHintEvent.
// Empty appID/nodeID is a silent no-op so a malformed wire
// payload doesn't poison the cache.
//
// Same-node writes are idempotent (they overwrite the existing
// entry with the same value). The stream only emits on actual
// changes per schedd-side filtering, so the no-op cases are
// rare in practice.
func (c *WarmHintCache) Update(appID, nodeID string) {
	if c == nil || appID == "" || nodeID == "" {
		return
	}
	c.mu.Lock()
	c.m[appID] = nodeID
	c.mu.Unlock()
}

// Forget drops the entry for appID. Reserved for a future
// "hint-clear-on-disconnect" follow-up — today's freeze policy
// (Phase 3 review) doesn't call this. Kept on the type so the
// stream consumer has a coherent API even when the policy flips.
func (c *WarmHintCache) Forget(appID string) {
	if c == nil || appID == "" {
		return
	}
	c.mu.Lock()
	delete(c.m, appID)
	c.mu.Unlock()
}

// Hint returns the cached nodeID for appID, or "" with
// found=false if no entry exists. This is the read path the
// picker takes on every Pick — kept tight (one RLock + map
// read) because it's on the request hot path.
//
// The returned (nodeID, found) matches the WarmHintFunc shape
// exactly so HintFunc can return it directly.
func (c *WarmHintCache) Hint(appID string) (string, bool) {
	if c == nil || appID == "" {
		return "", false
	}
	c.mu.RLock()
	n, ok := c.m[appID]
	c.mu.RUnlock()
	return n, ok
}

// HintFunc adapts the cache to the picker's WarmHintFunc
// signature (pkg/gateway/pgbackend.go:26). cmd/gatewayd wires
// this into PGBackend via WithWarmHint at backend construction
// time; the picker reads it on every Pick.
//
// HintFunc captures the cache pointer; the closure is safe for
// concurrent use because all underlying methods are locked.
func (c *WarmHintCache) HintFunc() WarmHintFunc {
	return c.Hint
}

// Len reports the current entry count. Test-only — production
// code should never need this.
func (c *WarmHintCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.m)
}
