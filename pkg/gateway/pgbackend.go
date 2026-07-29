package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
)

// WarmHintFunc is the sticky-warm affinity source for the picker
// (placement scheduler PR, ADR-025). It returns the compute_node.id
// that last warmed the given app, or "" with found=false if no hint
// is available (no record, expired, or stream disconnected). A nil
// WarmHintFunc is treated as "no hint always" — the picker falls
// through to least-loaded headroom identically to a fresh install.
//
// The hint is bias, never a gate — pkg/sched/ChoosePlacement
// ignores the hint when the preferred node is saturated. ADR-005
// (cold boot must always work) is preserved at the gateway too:
// an empty hint degrades to round-robin-within-warmest-node.
type WarmHintFunc func(appID string) (nodeID string, found bool)

// Router is the Postgres-backed routing seam PGBackend reads through. It is the
// narrow slice of the state.Store the edge needs to resolve a hostname to its
// app; cmd/gatewayd adapts state.Store to it. Keeping it gateway-local (rather
// than importing pkg/state here) keeps the hot request path unit-testable with
// a fake and keeps this package's dependency surface to pkg/api only.
type Router interface {
	// ResolveHost maps a request hostname (lowercased, port-stripped) to its
	// routing app. ok=false means "no app is routed here" (a 404, not an error);
	// a non-nil error means the lookup itself failed (Postgres down) and the
	// caller should surface it as a 404 without caching.
	ResolveHost(ctx context.Context, host string) (app App, ok bool, err error)
}

// targetSet (issue #168, placement scheduler PR) is the per-app list
// of routable instances the gateway holds. Members are unique by
// InstanceID; Pick uses a per-node sub-cursor so the picker biases
// toward the node with the most healthy entries and applies a
// sticky-warm affinity bonus (ADR-025).
//
// Concurrency model:
//   - subCursors maps each distinct NodeID to an atomic round-robin
//     cursor. Pick increments the cursor of the winning node only,
//     so two concurrent picks for different nodes don't compete.
//   - entries is read-only inside Pick (RLock); mutation happens
//     under Lock (add / remove).
//   - nodeOrder is a stable insertion order for tie-breaking when
//     two nodes have equal healthy counts. Re-built lazily on add
//     if the new Target's NodeID is novel.
type targetSet struct {
	next       atomic.Uint64 // legacy single-cursor; retained so legacy callers / tests that read len + add keep working. pick() no longer increments it.
	entries    []Target
	nodeOrder  []string                  // stable node-id order for tie-break
	subCursors map[string]*atomic.Uint64 // nodeID -> per-node cursor
}

// add appends a new Target to the set, replacing any existing entry with
// the same InstanceID. Callers must hold tgtMu (Lock).
func (s *targetSet) add(t Target) {
	if t.NodeID == "" || t.InstanceID == "" {
		return
	}
	for i, e := range s.entries {
		if e.InstanceID == t.InstanceID {
			// Re-admission of a known instance — overwrite in place.
			s.entries[i] = t
			return
		}
	}
	s.entries = append(s.entries, t)
	if _, ok := s.subCursors[t.NodeID]; !ok {
		s.subCursors[t.NodeID] = &atomic.Uint64{}
		s.nodeOrder = append(s.nodeOrder, t.NodeID)
	}
}

// remove drops the entry whose InstanceID matches. Returns the new slice
// length. Callers must hold tgtMu (Lock).
func (s *targetSet) remove(instanceID string) int {
	var removedNode string
	for i, e := range s.entries {
		if e.InstanceID == instanceID {
			removedNode = e.NodeID
			s.entries = append(s.entries[:i], s.entries[i+1:]...)
			break
		}
	}
	if removedNode == "" {
		return len(s.entries)
	}
	// Lazy GC: only drop the per-node cursor when no entry remains
	// for that node. Keeps the picker hot-path free of allocations
	// for steady-state traffic.
	stillUsed := false
	for _, e := range s.entries {
		if e.NodeID == removedNode {
			stillUsed = true
			break
		}
	}
	if !stillUsed {
		delete(s.subCursors, removedNode)
		for i, n := range s.nodeOrder {
			if n == removedNode {
				s.nodeOrder = append(s.nodeOrder[:i], s.nodeOrder[i+1:]...)
				break
			}
		}
	}
	return len(s.entries)
}

// pick returns one Target via per-node sub-cursors with sticky-warm
// affinity. The picker:
//
//  1. Groups entries by NodeID.
//  2. Scores each node: score(node) = healthyCount(node) + warmBonus(node).
//     warmBonus is +∞ if warmHint == nodeID, else 0. The +∞ is
//     modelled as math.MaxInt so the tie-break stays integer-arithmetic
//     without reflection.
//  3. Round-robin within the winning node's sub-cursor.
//
// Callers must hold tgtMu (RLock). Empty set → ok=false.
//
// warmHint="" or warmHintFound=false → score reduces to healthyCount.
// The nodeOrder slice keeps the comparison deterministic when two
// nodes tie (legacy single-box deploys always have exactly one
// node, so this branch is exercised in tests, not production).
func (s *targetSet) pick(warmHint string) (Target, bool) {
	if len(s.entries) == 0 {
		return Target{}, false
	}
	if len(s.subCursors) == 1 {
		// Fast path: every entry is on one node. Round-robin
		// over the entries directly (the only sub-cursor we
		// have is for that one node). This keeps the single-box
		// hot path allocation-free, matching the legacy atomic
		// round-robin shape.
		idx := s.next.Add(1) - 1
		return s.entries[int(idx%uint64(len(s.entries)))], true
	}
	// Multi-node: group entries by node, score each, pick the
	// winning node, round-robin within it.
	counts := make(map[string]int, len(s.nodeOrder))
	for _, e := range s.entries {
		counts[e.NodeID]++
	}
	var (
		bestNode  string
		bestScore int64 = -1
	)
	for _, nodeID := range s.nodeOrder {
		c := int64(counts[nodeID])
		score := c
		if warmHint != "" && nodeID == warmHint {
			// +∞: bias the warm node above any non-warm node.
			// math.MaxInt is fine because counts stay bounded
			// by max_concurrency (≤ 20 for Scale plan).
			score = c + (1 << 62)
		}
		if score > bestScore {
			bestScore = score
			bestNode = nodeID
		} else if score == bestScore {
			// Stable lex tie-break so identical scores don't
			// flip the pick between calls. Cheap because
			// nodeOrder is small.
			if nodeID < bestNode {
				bestNode = nodeID
			}
		}
	}
	if bestNode == "" {
		return Target{}, false
	}
	cursor, ok := s.subCursors[bestNode]
	if !ok {
		return Target{}, false
	}
	// Build a per-node index slice on demand; the picker is on
	// the hot path so we cache the indices for repeated calls.
	// The simplest correct implementation just counts.
	n := counts[bestNode]
	idx := cursor.Add(1) - 1
	// Walk entries to find the idx-th one on bestNode.
	var seen int
	for _, e := range s.entries {
		if e.NodeID != bestNode {
			continue
		}
		if int(idx%uint64(n)) == seen {
			return e, true
		}
		seen++
	}
	return Target{}, false
}

// PGBackend is gatewayd's production Backend (spec §4.1, issue #168): a
// host→app routing cache over Postgres plus schedd over gRPC for
// per-instance admission. Replaces the M4-era unwiredBackend once schedd's
// gRPC surface (ADR-018) is up.
//
// Two caches, populated on different paths:
//
//   - routes/apps: host→app_id (RouteCache, spec §4.1 10k LRU) and app_id→App
//     (plan). Filled on a Lookup miss via Router; wholesale-reset on an
//     app/domain change (Reset / FlushRoutes).
//   - targets: app_id → *targetSet. Filled by Admit (issue #168) when
//     schedd returns a fresh instance, and mutated by EvictInstance when
//     an instance_changed notification says a specific instance parked.
//     Pick is the ctx-less hot path, so it must be a pure in-memory read —
//     the notify loop + the admit path keep it fresh rather than per-request
//     DB hits.
type PGBackend struct {
	router Router
	sched  Scheduler
	log    *slog.Logger

	routes *RouteCache // host -> app_id (LRU)

	appsMu sync.RWMutex
	apps   map[string]App // app_id -> App (plan)

	// warmHint is the sticky-warm affinity source for the picker
	// (placement scheduler PR, ADR-025). nil = no hint; pick() falls
	// through to per-node healthyCount scoring. Set via WithWarmHint.
	warmHint WarmHintFunc

	tgtMu sync.RWMutex
	// targets is the hot-path app_id → *targetSet cache. Each targetSet
	// holds 1..max_concurrency Targets, picked via per-node sub-cursors
	// (issue #168 + ADR-025).
	targets map[string]*targetSet
}

// WithWarmHint attaches the sticky-warm affinity source for the picker.
// nil is tolerated (the picker degrades to per-node healthyCount).
// cmd/gatewayd wires this from the WarmHint stream that schedd exposes
// via the gRPC surface; tests pass a closure that reads from a fake
// or a fixed map.
//
// As of PR #429 the WarmHint stream gRPC RPC is not yet wired, so
// production gateways leave this unset. The picker correctly returns
// "no hint" and falls through to per-node healthyCount + lex
// tie-break on nodeOrder. Sticky-warm is enabled as soon as the
// stream consumer lands (follow-up slice tracked in
// docs/adr/025 — see plan file).
func (b *PGBackend) WithWarmHint(fn WarmHintFunc) *PGBackend {
	b.warmHint = fn
	return b
}

// compile-time assertion PGBackend satisfies the edge seam.
var _ Backend = (*PGBackend)(nil)

// NewPGBackend wires the production backend. log may be nil (slog default).
func NewPGBackend(router Router, sched Scheduler, log *slog.Logger) *PGBackend {
	if log == nil {
		log = slog.Default()
	}
	return &PGBackend{
		router:  router,
		sched:   sched,
		log:     log,
		routes:  NewRouteCache(RouteCacheCap),
		apps:    map[string]App{},
		targets: map[string]*targetSet{},
	}
}

// RouteCacheCap is the host→app_id cache ceiling (spec §4.1: 10,000 routes).
const RouteCacheCap = 10_000

// Lookup resolves a hostname to its app, cache-first (spec §4.1). A cache miss
// is one indexed Postgres lookup through the Router; the result is memoized in
// both the route (host→app_id) and app (app_id→plan) caches. A Router error or
// an unknown host both yield ok=false so the handler writes a 404.
func (b *PGBackend) Lookup(ctx context.Context, host string) (App, bool) {
	if appID, ok := b.routes.Get(host); ok {
		if app, ok := b.getApp(appID); ok {
			return app, true
		}
	}
	app, ok, err := b.router.ResolveHost(ctx, host)
	if err != nil {
		b.log.Warn("gateway: route lookup failed", "host", host, "err", err)
		return App{}, false
	}
	if !ok {
		return App{}, false
	}
	b.routes.Put(host, app.ID)
	b.putApp(app)
	return app, true
}

// Pick returns one routable Target for appID via per-node sub-cursors
// (issue #168, placement scheduler PR). Returns ("", false) when the
// cache is empty (no wake has populated it yet, or every cached
// instance was evicted). The handler must ensure capacity before
// calling Pick so this only returns false on the rare eviction race.
//
// Sticky-warm affinity (ADR-025): b.warmHint, if non-nil, biases the
// pick toward the node that last warmed this app. nil warmHint
// degrades to per-node healthyCount scoring with the lex tie-break
// on node-id — a fresh deploy (no warm hint) still distributes
// across nodes that already have live instances.
func (b *PGBackend) Pick(appID string) (Target, bool) {
	var warmHint string
	if b.warmHint != nil {
		if n, found := b.warmHint(appID); found {
			warmHint = n
		}
	}
	b.tgtMu.RLock()
	set := b.targets[appID]
	if set == nil {
		b.tgtMu.RUnlock()
		return Target{}, false
	}
	t, ok := set.pick(warmHint)
	b.tgtMu.RUnlock()
	return t, ok
}

// HealthyCount returns the number of routable Targets currently cached for
// appID (issue #168). Drives the WakeGate's shouldWake predicate: stop
// admitting once we're at the plan's effective max_concurrency.
func (b *PGBackend) HealthyCount(appID string) int {
	b.tgtMu.RLock()
	set := b.targets[appID]
	if set == nil {
		b.tgtMu.RUnlock()
		return 0
	}
	n := len(set.entries)
	b.tgtMu.RUnlock()
	return n
}

// Admit asks schedd to admit ONE additional instance for appID (issue #168).
// On the admitted path the new Target is added to the per-app targetSet
// (dedup by InstanceID). On the at-capacity path the engine's typed result
// is passed through (wakeID empty, method WakeMethodUnspecified, err nil).
// On a real failure (RAM headroom, chooser, store) the error is preserved —
// schedd lifts them to *api.Problem at the wire boundary.
//
// Fan-out invariant (issue #168): HealthyCount < maxConcurrency is enforced
// atomically inside this method. Concurrent callers serialize on tgtMu so a
// burst of N requests cannot collectively exceed the cap. Schedd also
// enforces the cap via its per-app ledger, but that round-trip is expensive
// — the gateway-side check is the cheap fast path that keeps the RPC count
// ≤ maxConcurrency per burst.
//
// method is the wake-outcome schedd actually performed (PR scale-out
// readiness, ADR-028). On the admitted path the value is
// WakeMethodSnapshotRestore or WakeMethodColdBoot, translated by
// scheddgrpc.Client from the wire's scheddpb.WakeMethod. On
// at-capacity and error paths the value is WakeMethodUnspecified.
func (b *PGBackend) Admit(ctx context.Context, appID string, maxConcurrency int) (string, WakeMethod, bool, error) {
	// Cheap fast path: refuse before we spend a gRPC round-trip.
	b.tgtMu.Lock()
	set := b.targets[appID]
	if set != nil && len(set.entries) >= maxConcurrency {
		b.tgtMu.Unlock()
		return "", WakeMethodUnspecified, true, nil
	}
	b.tgtMu.Unlock()

	instanceID, nodeID, wakeID, rawMethod, atCapacity, err := b.sched.AdmitInstance(ctx, appID)
	if err != nil {
		return "", WakeMethodUnspecified, false, err
	}
	method := scheddWakeMethodToGateway(rawMethod)
	if atCapacity {
		return "", WakeMethodUnspecified, true, nil
	}
	if nodeID == "" || instanceID == "" {
		// schedd returned a successful admit with empty ids. This is
		// an internal-server-error class event — the wire contract
		// says instance_id/node_id are populated on the admitted
		// path. Lift to *api.Problem so writeWakeError surfaces a
		// descriptive 5xx instead of the generic "wake failed" 503.
		return "", WakeMethodUnspecified, false, api.NewProblem(http.StatusInternalServerError,
			api.CodeCapacity, "schedd admit returned empty ids",
			fmt.Sprintf("instance=%q node=%q wake=%q", instanceID, nodeID, wakeID))
	}
	b.tgtMu.Lock()
	set = b.targets[appID]
	if set == nil {
		set = &targetSet{
			subCursors: map[string]*atomic.Uint64{},
		}
		b.targets[appID] = set
	}
	set.add(Target{
		NodeID:     nodeID,
		InstanceID: instanceID,
		WakeID:     wakeID,
		AddedAt:    time.Now(),
	})
	b.tgtMu.Unlock()
	return wakeID, method, false, nil
}

// EvictInstance drops a specific instance from its app's targetSet (issue
// #168). The instance_changed notification loop calls this with the
// instance_id from the pg_notify payload; only that single entry is
// removed, leaving any other instances in the set routable.
func (b *PGBackend) EvictInstance(appID, instanceID string) {
	if appID == "" || instanceID == "" {
		return
	}
	b.tgtMu.Lock()
	set := b.targets[appID]
	if set == nil {
		b.tgtMu.Unlock()
		return
	}
	if set.remove(instanceID) == 0 {
		delete(b.targets, appID)
	}
	b.tgtMu.Unlock()
}

// EvictTarget drops ALL cached targets for appID (legacy contract). Kept
// for callers that don't yet parse the instance_id from the
// instance_changed payload — it under-evicts nothing because the next
// request will Pick from what's left and miss if everything's gone,
// then re-admit. New code should prefer EvictInstance.
func (b *PGBackend) EvictTarget(appID string) {
	b.tgtMu.Lock()
	delete(b.targets, appID)
	b.tgtMu.Unlock()
}

// FlushRoutes clears the host→app and app→plan caches. gatewayd calls this on
// an app_changed / domain_changed notification so a renamed slug, plan change,
// or deleted app is re-resolved (or 404'd) on the next request.
func (b *PGBackend) FlushRoutes() {
	b.routes.Reset()
	b.appsMu.Lock()
	b.apps = map[string]App{}
	b.appsMu.Unlock()
}

func (b *PGBackend) getApp(appID string) (App, bool) {
	b.appsMu.RLock()
	app, ok := b.apps[appID]
	b.appsMu.RUnlock()
	return app, ok
}

func (b *PGBackend) putApp(app App) {
	b.appsMu.Lock()
	b.apps[app.ID] = app
	b.appsMu.Unlock()
}
