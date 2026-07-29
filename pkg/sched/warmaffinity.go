// warmaffinity.go — schedd's sticky-warm affinity cache (placement
// scheduler PR, ADR-025).
//
// The placement chooser biases a wake toward the compute node that last
// warmed the same app, so a hot app's snapshot + page cache stay warm
// across reaper cycles (ADR-009: snapshot reuse). The hint is a TTL'd
// in-memory map; schedd is the only writer and the only reader, so no
// DB traffic, no pg_notify, no concurrency outside the per-app lock.
//
// Cold-boot path (ADR-005) is preserved by construction: an empty
// LastWarmNode falls through to the least-loaded RAM headroom path,
// identical to a fresh install. Sticky-warm is bias, never a gate
// (pkg/sched/placement.go::ChoosePlacement ignores the hint when the
// preferred node is saturated).
//
// TTL matches api.WarmAffinityTTL (default 30 min). Overridable via
// the env var FAAS_WARM_AFFINITY_TTL on schedd startup; the engine
// constructor takes a duration so callers can stub it in tests.

package sched

import (
	"sync"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
)

// WarmAffinity is the sticky-warm cache. Safe for concurrent use;
// RecordWake takes the write lock, LastWarmNode takes the read lock
// with lazy TTL eviction. The map is bounded by Σ(appID), not by
// fleet size, so memory cost is small (one entry per hot app, with
// a string + string + time.Time payload).
//
// Eviction is lazy on read: a stale entry sits in the map until the
// next LastWarmNode for that app evicts it. A periodic sweeper is
// future work — the cardinality stays small in practice (handful of
// hot apps) so a small O(1) amortized cost is fine.
type WarmAffinity struct {
	mu  sync.RWMutex
	ttl time.Duration
	now func() time.Time // injected for tests; defaults to time.Now
	m   map[string]warmEntry
}

type warmEntry struct {
	nodeID   string
	lastSeen time.Time
}

// NewWarmAffinity returns a WarmAffinity with the given TTL. A zero or
// negative TTL falls back to api.WarmAffinityTTL (the single source of
// truth for the default — CLAUDE.md). The clock is time.Now by default;
// tests inject a fake clock via setClock (private — same package only).
func NewWarmAffinity(ttl time.Duration) *WarmAffinity {
	if ttl <= 0 {
		ttl = api.WarmAffinityTTL
	}
	return &WarmAffinity{
		ttl: ttl,
		now: time.Now,
		m:   make(map[string]warmEntry),
	}
}

// RecordWake stamps (appID → nodeID) with the current time. Callers
// should hold the Engine's per-app lock (matches choosePlacementLocked
// in pkg/sched/engine.go) so a concurrent wake for the same app can't
// race the Record. The map write itself is locked separately for
// safety — the per-app lock isn't load-bearing for correctness, just
// for "stick to the same node within one burst".
func (w *WarmAffinity) RecordWake(appID, nodeID string) {
	if w == nil || appID == "" || nodeID == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.m[appID] = warmEntry{nodeID: nodeID, lastSeen: w.now()}
}

// LastWarmNode returns the remembered nodeID for appID, or "" with
// found=false if no entry exists or the entry has expired. The read
// is O(1); expired entries are evicted in place (no second pass).
//
// Eviction policy: on stale, delete the entry so the next RecordWake
// doesn't waste a lookup on the same key. The map size is bounded
// by app count, so this is the right shape.
func (w *WarmAffinity) LastWarmNode(appID string) (string, bool) {
	if w == nil || appID == "" {
		return "", false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	e, ok := w.m[appID]
	if !ok {
		return "", false
	}
	if w.now().Sub(e.lastSeen) > w.ttl {
		delete(w.m, appID)
		return "", false
	}
	return e.nodeID, true
}

// Forget drops the entry for appID. Used by the engine when an app
// is force-parked or moved to STOPPED — the next wake shouldn't
// prefer a node the app hasn't run on recently. Idempotent.
func (w *WarmAffinity) Forget(appID string) {
	if w == nil || appID == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.m, appID)
}

// Len reports the current entry count. Test-only — production code
// should never need this.
func (w *WarmAffinity) Len() int {
	if w == nil {
		return 0
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	return len(w.m)
}

// setClock is the test-only clock injector. Same-package access only.
func (w *WarmAffinity) setClock(now func() time.Time) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if now != nil {
		w.now = now
	}
}
