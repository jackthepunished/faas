package gateway

// Edge Rules matcher surface (ADR-089 / issue #561, PR 3 + PR 4).
//
// PR 1 (#799) shipped the `edge_rules` table, state layer, apid CRUD,
// SDK, and OpenAPI. PR 2 (#815) shipped the `gregale edge-rules` CLI.
// PR 3 (#820) shipped the per-host LRU mirror of `RouteCache`
// (`pkg/gateway/routes.go`) holding the subset of each rule the matcher
// reads, plus two narrow interfaces the handler uses to inject
// `kind=route` substitution between the apps-suffix gate and
// `Backend.Lookup` at `pkg/gateway/handler.go:1609-1618`.
//
// PR 4 widens the matcher with three header-path kinds (rewrite /
// redirect / headers) — same forward-compatible interface shape: a
// `noOpEdgeRuleMatcher` embedded by future kinds gives the default
// no-op for free. The cache primitive widens to a per-host `hostEntry`
// that carries all four kinds' compiled slices; recompilation happens
// once on a miss for any kind (the SQL roundtrip dominates, so paying
// the compile cost four times is irrelevant).
//
// PR 5-7 add MatchCORS, MatchJWT, MatchIP on top of the same surface;
// the cache + invalidation plumbing stays unchanged.
//
// Why a subset type (not `state.EdgeRule`): pkg/gateway has no
// `pkg/state` import today and adding one would be a reverse dep
// (mirrors the existing `RequireAuthnAuthenticator` interface
// pattern at `pkg/gateway/handler.go:194-204`). The loader in
// `cmd/gatewayd-internal/edge_rules.go` is the single seam where
// `state.EdgeRule → EdgeRule*Resolved` happens.

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

// pathGlobError is the parse-failure tuple the loader threads back
// to the gateway hot path so an operator can diagnose a malformed
// glob via slog. Lives in pkg/gateway (rather than cmd-side) so
// the cache entry shape carries it directly. The cmd-side loader
// (cmd/gatewayd-internal/edge_rules.go) imports this type and
// populates the hostEntry.pathGlobErrs slice.
//
// The rule itself is silently dropped from the compiled slice —
// the customer sees a 404 (no match), which is the safest outcome
// (a malformed rule firing would steer traffic unpredictably).
// PR 3 review-fix R3 introduced this so the typo is operator-visible.
type pathGlobError struct {
	RuleID string
	Glob   string
	Err    error
}

// PathGlobError is the exported alias of pathGlobError so the
// cmd-side loader (which lives in a different package) can
// populate hostEntry.PathGlobErrs. Mirrors the unexported
// type's fields verbatim; the alias keeps the package-private
// discipline intact (the slice is constructed in pkg/gateway
// via cmd-side, but the type lives in pkg/gateway to avoid
// a circular import).
type PathGlobError = pathGlobError

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

// EdgeRuleRewriteResolved is the kind=rewrite subset. PR 4 mutates
// r.URL.Path in place when a rule matches (From prefix → To
// replacement; the spec §13.4 documents the "$1" capture shape
// for trailing-`*` From patterns — applied via stdlib path.Match
// + string replace at filter time).
type EdgeRuleRewriteResolved struct {
	ID        string
	AccountID string
	AppID     string
	Priority  int
	PathGlob  string          // "" = any path
	Methods   map[string]bool // nil = any method
	From      string          // literal prefix to strip; "" = match-any
	To        string          // replacement prefix; required when rule fires
}

// EdgeRuleRedirectResolved is the kind=redirect subset. PR 4 emits
// a 3xx via http.Redirect (stdlib) when a rule matches. StatusCode
// ∈ {301,302,307,308}; the loader defaults to 302 when 0. Headers
// are stamped on the response via w.Header().Set before the redirect.
type EdgeRuleRedirectResolved struct {
	ID         string
	AccountID  string
	AppID      string
	Priority   int
	PathGlob   string
	Methods    map[string]bool
	StatusCode int
	To         string
	Headers    map[string]string
}

// EdgeRuleHeaderOp is one mutation a kind=headers rule carries.
// Action ∈ {add, set, remove}; Value is ignored for "remove".
// Blacklist (Host, Content-Length, Transfer-Encoding, Connection,
// x-faas-*) is enforced at apid-Validate-time (PR 1).
type EdgeRuleHeaderOp struct {
	Name   string
	Value  string
	Action string
}

// EdgeRuleHeadersResolved is the kind=headers subset. PR 4 applies
// RequestHeaders to r BEFORE the proxy leg and ResponseHeaders to
// w BEFORE w.WriteHeader (mirrors the existing statusRecorder order).
// Ops apply in declared order (Cloudflare's "first wins" rule for
// `set`); the order is preserved from the customer's wire input.
type EdgeRuleHeadersResolved struct {
	ID              string
	AccountID       string
	AppID           string
	Priority        int
	PathGlob        string
	Methods         map[string]bool
	RequestHeaders  []EdgeRuleHeaderOp
	ResponseHeaders []EdgeRuleHeaderOp
}

// EdgeRuleCache is the in-memory per-host LRU (PR 3 shape; PR 4
// widens the entry to a HostEntry carrying four compiled slices —
// one per kind). Wholesale `Reset()` on `db.NotifyEdgeRuleChanged`
// is the only invalidation — single-box scale assumption per
// spec §4.3. Mirrors `RouteCache` at `pkg/gateway/routes.go:11-96`.
type EdgeRuleCache struct {
	mu   sync.Mutex
	cap  int
	ll   *list.List               // front = most recently used
	byID map[string]*list.Element // host → element (element.Value is *HostEntry)
}

// HostEntry is the per-host compiled-rule set the EdgeRuleCache
// LRU stores. cmd-side constructs one per cache miss via
// cmd/gatewayd-internal/edge_rules.go::loadHost, which runs every
// compile* over the same []state.EdgeRule slice and stitches the
// results into a single HostEntry. PathGlobErrs aggregates the
// path-glob parse errors from all four compileXxx calls so the
// loader can re-emit them at WARN on subsequent reads without
// re-running path.Match.
//
// Exported so the cmd-side loader can populate the cache via
// `cache.Put(host, &gateway.HostEntry{...})` — see how the cmd-side
// loadHost builds the entry. The PR 4 widening lives in this struct's
// per-kind slice fields; PR 5-7 widen with CORS / JWT / IP slots.
type HostEntry struct {
	Host         string
	Route        []EdgeRuleResolved
	Rewrite      []EdgeRuleRewriteResolved
	Redirect     []EdgeRuleRedirectResolved
	Headers      []EdgeRuleHeadersResolved
	PathGlobErrs []PathGlobError
}

// NewEdgeRuleCache returns a cache holding up to `capacity` host
// entries. A capacity < 1 is clamped to 1 (matches RouteCache).
func NewEdgeRuleCache(capacity int) *EdgeRuleCache {
	if capacity < 1 {
		capacity = 1
	}
	return &EdgeRuleCache{cap: capacity, ll: list.New(), byID: map[string]*list.Element{}}
}

// Get returns the cached `kind=route` slice for host and whether
// the entry was present, promoting the entry on hit. Returns
// (nil, false) when the host has no compiled entry OR the entry
// exists but the route slice is nil — both cases are a miss from
// the matcher's perspective.
//
// Returns a value-copy of the underlying slice so callers can
// mutate without poisoning the cache (mirrors the RouteCache
// value-copy pattern at `pkg/gateway/routes.go`).
func (c *EdgeRuleCache) Get(host string) ([]EdgeRuleResolved, bool) {
	entry, ok := c.getEntry(host)
	if !ok || entry.Route == nil {
		return nil, false
	}
	src := entry.Route
	out := make([]EdgeRuleResolved, len(src))
	copy(out, src)
	return out, true
}

// GetRewrite / GetRedirect / GetHeaders are the PR 4 per-kind
// accessors mirroring Get. Each returns a value-copy of the
// underlying slice and a hit bool; nil slice with ok=true means
// "entry exists but no rule of this kind for this host".
func (c *EdgeRuleCache) GetRewrite(host string) ([]EdgeRuleRewriteResolved, bool) {
	entry, ok := c.getEntry(host)
	if !ok {
		return nil, false
	}
	if entry.Rewrite == nil {
		return nil, true
	}
	src := entry.Rewrite
	out := make([]EdgeRuleRewriteResolved, len(src))
	copy(out, src)
	return out, true
}

func (c *EdgeRuleCache) GetRedirect(host string) ([]EdgeRuleRedirectResolved, bool) {
	entry, ok := c.getEntry(host)
	if !ok {
		return nil, false
	}
	if entry.Redirect == nil {
		return nil, true
	}
	src := entry.Redirect
	out := make([]EdgeRuleRedirectResolved, len(src))
	copy(out, src)
	return out, true
}

func (c *EdgeRuleCache) GetHeaders(host string) ([]EdgeRuleHeadersResolved, bool) {
	entry, ok := c.getEntry(host)
	if !ok {
		return nil, false
	}
	if entry.Headers == nil {
		return nil, true
	}
	src := entry.Headers
	out := make([]EdgeRuleHeadersResolved, len(src))
	copy(out, src)
	return out, true
}

// getEntry promotes the entry on hit and returns it. Internal —
// the Get* family wraps this so each returns a typed slice.
func (c *EdgeRuleCache) getEntry(host string) (*HostEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.byID[host]
	if !ok {
		return nil, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*HostEntry), true
}

// Put replaces the entire hostEntry for host with the supplied
// compiled slices, evicting the LRU entry if the cache is over
// capacity. A nil entry is a no-op (the next Get returns a
// miss and the loader re-hits PG). PR 8 may add a negative-cache
// sentinel for hosts with zero active rules — deferred because
// the cache is advisory and a missing entry costs one indexed
// PG read (~0.5ms warm).
//
// All four slices are populated from the SAME PG read (a miss for
// any kind re-runs the SQL query and recompiles every kind); this
// is the loader's contract (cmd/gatewayd-internal/edge_rules.go).
func (c *EdgeRuleCache) Put(host string, e *HostEntry) {
	if e == nil {
		return
	}
	// PR 4 contract: a Put whose HostEntry has empty/nil slices for
	// every kind is a no-op (the loader re-hits PG on the next Get).
	// Pinning this so a future test or refactor can't silently start
	// caching "no rules" entries (which would mask a loader bug).
	if len(e.Route) == 0 && len(e.Rewrite) == 0 &&
		len(e.Redirect) == 0 && len(e.Headers) == 0 {
		return
	}
	e.Host = host
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.byID[host]; ok {
		el.Value = e
		c.ll.MoveToFront(el)
		return
	}
	el := c.ll.PushFront(e)
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
	delete(c.byID, el.Value.(*HostEntry).Host)
}

// EdgeRuleMatcher is the narrow contract the gateway handler uses
// to consult per-request edge rules. Implementations MUST be safe
// for concurrent use. PR 4 widens with MatchRewrite / MatchRedirect
// / MatchHeaders; PR 5-7 add MatchCORS / MatchJWT / MatchIP on top
// of the same shape. Future kind matchers embed a `noOpEdgeRuleMatcher`
// to inherit the no-op default.
//
// MatchRoute returns the highest-priority `kind=route` rule whose
// host, path, and method match the inbound request, or nil if no
// rule applies. The caller (handler.go) substitutes the resolved
// target App for the inbound-host's App before downstream gates run.
//
// MatchRewrite / MatchRedirect / MatchHeaders are the PR 4 per-kind
// matchers. Same priority-ASC + path/methods filter shape as
// MatchRoute; each returns the highest-priority rule of its kind
// that matches the inbound (host, path, method), or nil on miss.
//
// Reset drops every cached entry. Called by the gatewayd notify
// loop on `db.NotifyEdgeRuleChanged`.
type EdgeRuleMatcher interface {
	MatchRoute(ctx context.Context, host, path, method string) *EdgeRuleResolved
	MatchRewrite(ctx context.Context, host, path, method string) *EdgeRuleRewriteResolved
	MatchRedirect(ctx context.Context, host, path, method string) *EdgeRuleRedirectResolved
	MatchHeaders(ctx context.Context, host, path, method string) *EdgeRuleHeadersResolved
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
// embed it (PR 4 ships MatchRoute + MatchRewrite + MatchRedirect +
// MatchHeaders); the type exists for the forward-compatible
// interface shape so a future CORS / JWT / IP impl can embed it
// and only override the kinds it ships.
type noOpEdgeRuleMatcher struct{}

func (noOpEdgeRuleMatcher) MatchRoute(context.Context, string, string, string) *EdgeRuleResolved {
	return nil
}
func (noOpEdgeRuleMatcher) MatchRewrite(context.Context, string, string, string) *EdgeRuleRewriteResolved {
	return nil
}
func (noOpEdgeRuleMatcher) MatchRedirect(context.Context, string, string, string) *EdgeRuleRedirectResolved {
	return nil
}
func (noOpEdgeRuleMatcher) MatchHeaders(context.Context, string, string, string) *EdgeRuleHeadersResolved {
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
	return pickFirstMatch(rules, path, method)
}

// PickFirstRewriteMatch / PickFirstRedirectMatch / PickFirstHeadersMatch
// are the PR 4 per-kind mirrors of PickFirstRouteMatch. Same shape:
// priority-ASC walk, methods filter (O(1) via map lookup), path glob
// via path.Match. Each is called from its own gatewaydEdgeRules
// Match* method after the cache returns the priority-ordered slice.
// Generics are deliberately avoided here — three small private copies
// keep the filter logic in pkg/gateway unit tests without exposing a
// generic helper the wider codebase doesn't need.
//
// Exported so the cmd-side loader (cmd/gatewayd-internal/edge_rules.go)
// can call them without poking at unexported helpers.
func PickFirstRewriteMatch(rules []EdgeRuleRewriteResolved, path, method string) *EdgeRuleRewriteResolved {
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

func PickFirstRedirectMatch(rules []EdgeRuleRedirectResolved, path, method string) *EdgeRuleRedirectResolved {
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

func PickFirstHeadersMatch(rules []EdgeRuleHeadersResolved, path, method string) *EdgeRuleHeadersResolved {
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

// pickFirstMatch is the route-specific helper. Generic over the
// resolved type via the small interface below so we avoid three
// near-identical loops in the common-case MatchRoute path.
//
// All four pick* helpers (pickFirstMatch for route +
// pickFirstRewriteMatch / pickFirstRedirectMatch / pickFirstHeadersMatch)
// share the same filter semantics — methods map, path glob, first hit.
// Splitting them keeps the per-kind return types precise without
// paying for a runtime-type assertion on every request.
func pickFirstMatch(rules []EdgeRuleResolved, path, method string) *EdgeRuleResolved {
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
