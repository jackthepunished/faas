// leader_resolver_pg.go — production CachedLeaderResolver.
//
// Tier A9 / ADR-084 §Decision #6: the writeGate sits on the
// mutating request hot path, so the leader lookup MUST NOT
// round-trip Postgres on every request. The cache is refreshed
// asynchronously by `LeaderURLPublisher` (cmd/gatewayd-internal/
// leader_url_publisher.go, PR-B sub-task B4) on
// `compute_node_changed` pg_notify events, AND on a 5 s TTL
// fallback (pkg/api/limits.go::StandbyWriteLeaderURLCacheTTLSeconds).
//
// # Concurrency
//
// Two concurrency axes:
//
//  1. **Stampede prevention.** N goroutines hitting the gate
//     while the cache is cold MUST NOT issue N parallel
//     `ListActiveComputeNodes` calls. The resolver uses
//     `golang.org/x/sync/singleflight` to coalesce — one
//     goroutine refreshes, others wait on the same done
//     channel. After the refresh, every waiter observes the
//     same answer via the cache (no double round-trip).
//  2. **Cache freshness.** A refresh triggered by a pg_notify
//     event MUST propagate to subsequent callers within the
//     same hot-path call. The mutex is held briefly (just to
//     install the new snapshot); readers use RLock for the
//     common path.
//
// # Error semantics
//
// `Current` returns `err` ONLY for cache-internal failures
// (the publisher died, the store returned an error during the
// last refresh). The cached `name`/`isMe` pair is the truth
// the gate reads from; an err with cached name="node-a"
// means "we still trust node-a, but the last refresh attempt
// failed — escalate via logs". The writeGate maps this to
// `OutcomeLeaderUnreachable` per the user decision table in
// noble-swimming-balloon.md §"User decisions" (PR-B preamble)
// — the gate treats any non-nil err the same as `name == ""`.
//
// # Why a separate file (not writegate.go)
//
// The writegate package keeps its surface pure (no I/O, no
// goroutines). The production resolver breaks that discipline
// — singleflight + RLock + store I/O — so it lives in its
// own file, marked with the load-bearing comment above. Tests
// in `leader_resolver_pg_test.go` exercise the cache, the
// singleflight coalescing, and the error path; the existing
// `fakeLeaderResolver` in `writegate_test.go` continues to
// drive the gate's request-level tests.
package writegate

import (
	"context"
	"sync"
	"time"

	"github.com/onebox-faas/faas/pkg/gateway/leader"
	"golang.org/x/sync/singleflight"
)

// CachedLeaderResolver is the production LeaderResolver
// implementation. It is goroutine-safe; every method can be
// called from multiple goroutines concurrently.
//
// Lifecycle:
//
//	store     leader.LeaderStore   — production = pkg/state.PGStore
//	nodeName  string               — local compute_nodes.name
//	cacheTTL  time.Duration        — typically 5 s (limits.go)
//	refresh   <-chan struct{}      — closed-by-publisher refresh edge
//
// The refresh channel is the only state mutation that does not
// hold the mutex — the publisher's `SendRefresh` simply
// non-blockingly deposits a struct into the channel. The
// resolver drains it via `drainRefresh` (called from Current)
// and forces a refresh on the next call. A buffered channel
// of size 1 is sufficient: the resolver drains synchronously,
// and any additional signals while a refresh is in-flight are
// collapsed (the in-flight refresh will pick up the LATEST
// store state).
type CachedLeaderResolver struct {
	store    leader.LeaderStore
	nodeName string
	cacheTTL time.Duration
	refresh  <-chan struct{}

	sfGroup singleflight.Group

	mu       sync.RWMutex
	cached   leader.Leader
	cachedAt time.Time
}

// NewCachedLeaderResolver constructs the resolver. The caller
// owns the refresh channel — typically created by the wiring
// code in cmd/gatewayd-internal/run.go as a `make(chan
// struct{}, 1)` and handed to both the resolver and the
// publisher.
//
// cacheTTL of 0 is treated as "always refresh" — useful for
// tests but never wired in production. PR-B's `run.go` reads
// `pkg/api/limits.go::StandbyWriteLeaderURLCacheTTLSeconds`
// (5 s) at boot.
func NewCachedLeaderResolver(
	store leader.LeaderStore,
	nodeName string,
	cacheTTL time.Duration,
	refresh <-chan struct{},
) *CachedLeaderResolver {
	return &CachedLeaderResolver{
		store:    store,
		nodeName: nodeName,
		cacheTTL: cacheTTL,
		refresh:  refresh,
	}
}

// Current implements LeaderResolver.
//
// Order of operations:
//
//  1. Drain the refresh channel (non-blocking) — a pending
//     signal means "force a refresh on this call, don't serve
//     stale". A drained signal arms the `forceRefresh` flag,
//     which overrides the TTL check in step 2.
//  2. Cache hit (snapshot fresh AND no force flag) — return
//     the cached pair.
//  3. Cache miss / expired / forced — singleflight refresh;
//     coalesced callers share the same store query.
//
// `err` is non-nil ONLY when the underlying store returns an
// error during a refresh. A successful refresh with
// `len(active) == 0` returns `(name="", isMe=false, err=nil)`
// — the gate maps that to OutcomeLeaderUnreachable via the
// `name == ""` branch, not the err branch.
func (r *CachedLeaderResolver) Current(ctx context.Context) (string, bool, error) {
	// Step 1: drain a pending publisher refresh signal.
	// Non-blocking; if the channel is empty, skip straight
	// to the cache check. A drained signal arms the
	// `forceRefresh` flag below.
	forceRefresh := r.drainRefresh()

	// Step 2: cache hit. The TTL check is inside the RLock so
	// a concurrent refresh that installs a newer snapshot is
	// observed immediately on the next call (the next call's
	// RLock sees the new cachedAt).
	r.mu.RLock()
	if !forceRefresh && !r.cached.IsZero() && r.cacheTTL > 0 && time.Since(r.cachedAt) < r.cacheTTL {
		name, isMe := r.cached.Name, r.cached.Name == r.nodeName
		r.mu.RUnlock()
		return name, isMe, nil
	}
	r.mu.RUnlock()

	// Step 3: refresh via singleflight. The closure captures
	// `r` by pointer (it is goroutine-safe by construction —
	// the mutex serializes the cache write).
	v, err, _ := r.sfGroup.Do("current", func() (interface{}, error) {
		// Re-check the cache inside the singleflight: a
		// peer goroutine that won the race may have
		// already refreshed while we were queued. BUT a
		// drained publisher signal still bypasses —
		// otherwise the herd above us collapses onto a
		// stale snapshot that just lost its TTL window.
		r.mu.RLock()
		if !forceRefresh && !r.cached.IsZero() && r.cacheTTL > 0 && time.Since(r.cachedAt) < r.cacheTTL {
			snap := r.cached
			r.mu.RUnlock()
			return snap, nil
		}
		r.mu.RUnlock()

		// Actually refresh. ctx propagates so a slow PG
		// query can be cancelled by the gate (the gate
		// uses a request-scoped ctx on every Current call).
		if err := ctx.Err(); err != nil {
			return leader.Leader{}, err
		}
		winner, err := leader.ElectLeader(ctx, r.store)
		if err != nil {
			return leader.Leader{}, err
		}

		// Install the new snapshot under Lock. Two
		// concurrent refreshers (theoretically impossible
		// because singleflight collapses them, but
		// defensive) serialize here.
		r.mu.Lock()
		r.cached = winner
		r.cachedAt = time.Now().UTC()
		r.mu.Unlock()
		return winner, nil
	})
	if err != nil {
		return "", false, err
	}
	snap, ok := v.(leader.Leader)
	if !ok {
		// singleflight contract violation — should never
		// happen because the closure always returns a
		// leader.Leader. Defensive: treat as error.
		return "", false, errCacheShape
	}
	return snap.Name, snap.Name == r.nodeName, nil
}

// drainRefresh consumes a pending publisher signal without
// blocking. The channel is buffered to size 1 by the wiring
// code; if it's empty (publisher hasn't signaled since the
// last drain), this is a no-op.
//
// Returns true iff a signal was drained — meaning the caller
// MUST bypass the TTL check and force a refresh. A signal
// represents a pg_notify event that the publisher already
// committed to; serving stale would defeat the entire
// purpose of the cache.
//
// Implementation note: this is the canonical "drain a
// signal-only channel" pattern. `select { case <-r.refresh:
// default: }` is the well-known idiom; documented here for
// reviewers not familiar with single-signal-channel design.
func (r *CachedLeaderResolver) drainRefresh() bool {
	select {
	case <-r.refresh:
		return true
	default:
		return false
	}
}

// errCacheShape is the defensive sentinel returned when
// singleflight hands us a non-Leader value (impossible by
// construction, but type-assertion in Go is unchecked and
// the gate's classification branches on err being non-nil).
var errCacheShape = &cacheShapeError{}

type cacheShapeError struct{}

func (*cacheShapeError) Error() string {
	return "writegate: CachedLeaderResolver singleflight returned non-Leader value"
}
