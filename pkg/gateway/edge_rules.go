package gateway

// Edge Rules matcher surface (ADR-089 / issue #561, PR 3).
//
// PR 1 (#799) shipped the `edge_rules` table, state layer, apid CRUD,
// SDK, and OpenAPI. PR 2 (#815) shipped the `gregale edge-rules` CLI.
// This file is PR 3's hot-path half: a per-host LRU mirror of
// `RouteCache` (`pkg/gateway/routes.go`) holding the subset of each
// rule the matcher reads, plus two narrow interfaces the handler
// uses to inject `kind=route` substitution between the apps-suffix
// gate and `Backend.Lookup` at `pkg/gateway/handler.go:1449-1451`.
//
// Why a subset type (not `state.EdgeRule`): pkg/gateway has no
// `pkg/state` import today and adding one would be a reverse dep
// (mirrors the existing `RequireAuthnAuthenticator` interface
// pattern at `pkg/gateway/handler.go:194-204`). The loader in
// `cmd/gatewayd-internal/edge_rules.go` is the single seam where
// `state.EdgeRule → EdgeRuleResolved` happens.
//
// PR 4-7 widen `EdgeRuleMatcher` with `MatchRewrite`, `MatchRedirect`,
// `MatchHeaders`, `MatchCORS`, `MatchJWT`, `MatchIP`. The interface
// shape is forward-compatible: a `noOpEdgeRuleMatcher` embedded by
// future kinds gives the default no-op for free.

import (
	"container/list"
	"context"
	"path"
	"sync"
)

// pathMatch is the stdlib path.Match wrapper — aliased here so the
// path-glob filter unit tests can stub it via build tags in the
// future without changing the production call site. Today it is a
// straight passthrough; the indirection documents the seam.
var pathMatch = path.Match

// EdgeRuleCacheCap is the maximum number of host entries kept in
// the in-memory LRU. Mirrors the spec §4.1 routing-cache capacity
// (10,000). Single-box scale makes wholesale Reset() cheaper than
// per-host tracking; gatewayd-internal calls Reset() on
// `db.NotifyEdgeRuleChanged`.
const EdgeRuleCacheCap = 10_000

// EdgeRuleResolved is the gateway-side subset of `state.EdgeRule`
// the matcher reads on every request. Fields are the minimum needed
// to (a) find a rule whose host matches, (b) apply the path/methods
// filter in Go, (c) resolve the target app via the closure injected
// at `WithEdgeRules`, (d) emit the audit row. Action JSON, full
// AppID, deployment data, and timestamps are intentionally absent —
// PR 4-7's per-kind actions read them out of `state.EdgeRule` again
// at the kind-specific code path.
type EdgeRuleResolved struct {
	ID            string
	AccountID     string
	AppID         string
	Priority      int
	PathGlob      string          // compiled via path.Match; "" = any path
	Methods       map[string]bool // empty = any method
	TargetAppSlug string          // kind=route only; ignored by PR 4-7
}

// EdgeRuleCache is the in-memory `host → []EdgeRuleResolved` LRU
// (mirrors `RouteCache` at `pkg/gateway/routes.go:11-96`). Wholesale
// `Reset()` on `db.NotifyEdgeRuleChanged` is the only invalidation
// — single-box scale assumption per spec §4.3.
type EdgeRuleCache struct {
	mu   sync.Mutex
	cap  int
	ll   *list.List               // front = most recently used
	byID map[string]*list.Element // host → element
}

type edgeRuleCacheEntry struct {
	host  string
	rules []EdgeRuleResolved
}

// NewEdgeRuleCache returns a cache holding up to `capacity` host
// entries. A capacity < 1 is clamped to 1 (matches RouteCache).
func NewEdgeRuleCache(capacity int) *EdgeRuleCache {
	if capacity < 1 {
		capacity = 1
	}
	return &EdgeRuleCache{cap: capacity, ll: list.New(), byID: map[string]*list.Element{}}
}

// Get returns the cached rules for host and whether the entry was
// present, promoting the entry on hit.
func (c *EdgeRuleCache) Get(host string) ([]EdgeRuleResolved, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.byID[host]; ok {
		c.ll.MoveToFront(el)
		// Return a copy so callers can mutate without poisoning
		// the cache slice (mirrors the RouteCache value-copy
		// pattern that pkg/gateway/handler.go:660-665 relies on).
		src := el.Value.(*edgeRuleCacheEntry).rules
		out := make([]EdgeRuleResolved, len(src))
		copy(out, src)
		return out, true
	}
	return nil, false
}

// Put inserts or replaces the rules for host, evicting the LRU
// entry if the cache is over capacity. A nil/empty rules slice is
// a no-op (the next Get returns a miss and the loader re-hits PG).
// PR 8 may add a negative-cache sentinel for hosts with zero
// active rules — deferred because the cache is advisory and a
// missing entry costs one indexed PG read (~0.5ms warm).
func (c *EdgeRuleCache) Put(host string, rules []EdgeRuleResolved) {
	if len(rules) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	dst := make([]EdgeRuleResolved, len(rules))
	copy(dst, rules)
	if el, ok := c.byID[host]; ok {
		el.Value.(*edgeRuleCacheEntry).rules = dst
		c.ll.MoveToFront(el)
		return
	}
	el := c.ll.PushFront(&edgeRuleCacheEntry{host: host, rules: dst})
	c.byID[host] = el
	if c.ll.Len() > c.cap {
		c.evictLRU()
	}
}

// Reset drops every cached entry. gatewayd-internal calls this on
// `db.NotifyEdgeRuleChanged` (mirrors `PGBackend.FlushRoutes`).
func (c *EdgeRuleCache) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ll.Init()
	c.byID = map[string]*list.Element{}
}

// Len returns the number of cached host entries.
func (c *EdgeRuleCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

func (c *EdgeRuleCache) evictLRU() {
	if el := c.ll.Back(); el != nil {
		c.removeElement(el)
	}
}

func (c *EdgeRuleCache) removeElement(el *list.Element) {
	c.ll.Remove(el)
	delete(c.byID, el.Value.(*edgeRuleCacheEntry).host)
}

// EdgeRuleMatcher is the narrow contract the gateway handler uses
// to consult per-request edge rules. Implementations MUST be safe
// for concurrent use. PR 4-7 widen this with MatchRewrite,
// MatchRedirect, MatchHeaders, MatchCORS, MatchJWT, MatchIP —
// future kind matchers embed a `noOpEdgeRuleMatcher` to inherit
// the no-op default.
//
// MatchRoute returns the highest-priority `kind=route` rule whose
// host, path, and method match the inbound request, or nil if no
// rule applies. The caller (handler.go) substitutes the resolved
// target App for the inbound-host's App before downstream gates run.
//
// Reset drops every cached entry. Called by the gatewayd notify
// loop on `db.NotifyEdgeRuleChanged`.
type EdgeRuleMatcher interface {
	MatchRoute(ctx context.Context, host, path, method string) *EdgeRuleResolved
	Reset()
}

// EdgeRuleAuditor is the narrow emit-only slice the matcher uses
// to record rule firings. Declared locally so pkg/gateway doesn't
// import cmd/* (avoid a reverse dep) and so tests can inject a
// counting fake. Best-effort semantics — the matcher never blocks
// a request on a failed emit (mirrors RequireAuthnAuditor at
// `pkg/gateway/handler.go:194-204`).
type EdgeRuleAuditor interface {
	Emit(ctx context.Context, kind string, subject *string, data map[string]any)
}

// noOpEdgeRuleMatcher is the default Embedding target PR 4-7 uses
// to inherit default no-op behavior for the kinds they don't ship.
// Today's production impl `cmd/gatewayd-internal.edgeRules` doesn't
// embed it (PR 3 ships MatchRoute only); the type exists for the
// forward-compatible interface shape.
type noOpEdgeRuleMatcher struct{}

func (noOpEdgeRuleMatcher) MatchRoute(context.Context, string, string, string) *EdgeRuleResolved {
	return nil
}
func (noOpEdgeRuleMatcher) Reset() {}

// ResolveTargetApp is the `AppBySlug` closure the matcher needs to
// swap the gateway.App when a `kind=route` rule fires. Production
// wraps `state.Store.AppBySlug` (`pkg/state/store.go:890`) at
// `cmd/gatewayd-internal/run.go`. The closure returns `(App{}, false)`
// when the slug is not found or the target is cross-account — the
// matcher emits `edge_rule.route_blocked` audit + `outcome=blocked`
// metric in that case (defense-in-depth on top of the apid create-
// time same-account guarantee at
// `cmd/apid/handlers_edge_rules.go:184-201`).
type ResolveTargetApp func(ctx context.Context, slug string) (App, bool)

// PickFirstRouteMatch is the pure-Go filter used by
// cmd/gatewayd-internal/edge_rules.go::gatewaydEdgeRules.MatchRoute
// after the cache returns the priority-ordered slice. Exported so
// the production loader (cmd-side) can call it without poking at
// the unexported helper, and pinned in pkg/gateway unit tests so
// the production filter can't silently drift. Walks the slice
// priority-ASC (lower number = earlier = first-match-wins) and
// returns the first rule whose path glob + methods filter match.
//
// methods filter:
//
//   - empty map = any method matches
//   - non-empty map = request method MUST be present (case-folded
//     to upper by the loader; HTTP method names are
//     case-sensitive per RFC 7231 §4.1 but the gateway stores
//     them upper-cased via state.EdgeRuleResponse.MatchMethods)
//   - the request's own method is matched as-given (the handler
//     passes r.Method which Go returns upper-case)
//
// path glob: passed through stdlib path.Match; "" = match all;
// "*" = match all; "/api/*" = prefix-wildcard on the second
// segment.
func PickFirstRouteMatch(rules []EdgeRuleResolved, path, method string) *EdgeRuleResolved {
	for i := range rules {
		r := &rules[i]
		if r.Methods != nil && !r.Methods[method] {
			continue
		}
		if r.PathGlob != "" {
			ok, _ := pathGlobMatch(r.PathGlob, path)
			if !ok {
				continue
			}
		}
		return r
	}
	return nil
}

// pathGlobMatch is a tiny adapter over stdlib path.Match that
// honours the two sentinel patterns the gateway rules allow:
// "" (any path) and "*" (any path). Stdlib path.Match treats
// both as errors for the empty / star input, so we short-circuit.
func pathGlobMatch(glob, p string) (bool, error) {
	if glob == "" || glob == "*" {
		return true, nil
	}
	return pathMatch(glob, p)
}
