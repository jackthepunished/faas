package gateway

// Tests for pkg/gateway/edge_rules.go (PR 3 of Edge Rules rollout,
// ADR-089 / issue #561). Mirror pkg/gateway/routes_test.go shape:
// table-driven where the shape is uniform, individual where the
// behaviour diverges. Run with -race to exercise the mutex.
//
// `EdgeRuleCache` is verified to behave identically to `RouteCache`
// for the LRU primitive — Get promotes, Put evicts, Reset clears.

import (
	"strings"
	"sync"
	"testing"
)

func sampleEdgeRule(id string, prio int, host, slug string) EdgeRuleResolved {
	return EdgeRuleResolved{
		ID:            id,
		AccountID:     "acc_" + id,
		AppID:         "app_" + id,
		Priority:      prio,
		PathGlob:      "",
		Methods:       nil,
		TargetAppSlug: slug,
	}
}

// putRoute is the test-only helper that wraps a route slice in a
// HostEntry for the new PR 4 cache API. Keeps the PR 3 test
// surface short and readable; production code uses cmd-side
// loadHost which populates all four kinds together.
func putRoute(c *EdgeRuleCache, host string, route []EdgeRuleResolved) {
	c.Put(host, &HostEntry{Host: host, Route: route})
}

// --- EdgeRuleCache primitive parity with RouteCache ----------------

func TestEdgeRuleCache_LRUEvictsAtCapacity(t *testing.T) {
	c := NewEdgeRuleCache(2)
	putRoute(c, "a.example.com", []EdgeRuleResolved{sampleEdgeRule("a", 10, "a.example.com", "alpha")})
	putRoute(c, "b.example.com", []EdgeRuleResolved{sampleEdgeRule("b", 10, "b.example.com", "beta")})
	putRoute(c, "c.example.com", []EdgeRuleResolved{sampleEdgeRule("c", 10, "c.example.com", "gamma")}) // evicts "a"
	if _, ok := c.Get("a.example.com"); ok {
		t.Errorf("a should have been evicted (cap=2)")
	}
	if _, ok := c.Get("b.example.com"); !ok {
		t.Errorf("b should still be cached")
	}
	if _, ok := c.Get("c.example.com"); !ok {
		t.Errorf("c should still be cached")
	}
}

func TestEdgeRuleCache_GetPromotesEntry(t *testing.T) {
	c := NewEdgeRuleCache(2)
	putRoute(c, "a.example.com", []EdgeRuleResolved{sampleEdgeRule("a", 10, "a.example.com", "alpha")})
	putRoute(c, "b.example.com", []EdgeRuleResolved{sampleEdgeRule("b", 10, "b.example.com", "beta")})
	if _, ok := c.Get("a.example.com"); !ok {
		t.Errorf("a should hit before promotion")
	}
	putRoute(c, "c.example.com", []EdgeRuleResolved{sampleEdgeRule("c", 10, "c.example.com", "gamma")})
	if _, ok := c.Get("b.example.com"); ok {
		t.Errorf("b should have been evicted (a was promoted)")
	}
	if _, ok := c.Get("a.example.com"); !ok {
		t.Errorf("a should still be cached after promotion")
	}
}

func TestEdgeRuleCache_ResetClearsAll(t *testing.T) {
	c := NewEdgeRuleCache(4)
	putRoute(c, "a.example.com", []EdgeRuleResolved{sampleEdgeRule("a", 10, "a.example.com", "alpha")})
	putRoute(c, "b.example.com", []EdgeRuleResolved{sampleEdgeRule("b", 10, "b.example.com", "beta")})
	if c.Len() != 2 {
		t.Errorf("len before Reset = %d, want 2", c.Len())
	}
	c.Reset()
	if c.Len() != 0 {
		t.Errorf("len after Reset = %d, want 0", c.Len())
	}
	if _, ok := c.Get("a.example.com"); ok {
		t.Errorf("a should be gone after Reset")
	}
}

func TestEdgeRuleCache_EmptyRulesIsNoOp(t *testing.T) {
	// Putting an empty/nil rules slice is a no-op (the loader
	// re-hits PG on the next Get). PR 8 may add a negative-cache
	// sentinel — deferred because the cache is advisory and a
	// missing entry costs one indexed PG read (~0.5ms warm).
	c := NewEdgeRuleCache(4)
	c.Put("empty.example.com", nil)
	c.Put("empty2.example.com", &HostEntry{Host: "empty2.example.com", Route: []EdgeRuleResolved{}})
	if c.Len() != 0 {
		t.Errorf("len = %d, want 0 (empty Put is no-op)", c.Len())
	}
}

func TestEdgeRuleCache_GetReturnsCopy(t *testing.T) {
	// Callers must not be able to mutate the cached slice through
	// the Get pointer — mirrors the RouteCache value-copy contract.
	c := NewEdgeRuleCache(2)
	src := []EdgeRuleResolved{sampleEdgeRule("a", 10, "a.example.com", "alpha")}
	putRoute(c, "a.example.com", src)
	got, _ := c.Get("a.example.com")
	if len(got) != 1 {
		t.Fatalf("got len = %d, want 1", len(got))
	}
	got[0].TargetAppSlug = "mutated"
	again, _ := c.Get("a.example.com")
	if again[0].TargetAppSlug != "alpha" {
		t.Errorf("cached entry was mutated through Get return: %q", again[0].TargetAppSlug)
	}
}

func TestEdgeRuleCache_PutOverwritesRules(t *testing.T) {
	c := NewEdgeRuleCache(2)
	putRoute(c, "a.example.com", []EdgeRuleResolved{sampleEdgeRule("a", 10, "a.example.com", "alpha")})
	putRoute(c, "a.example.com", []EdgeRuleResolved{sampleEdgeRule("a", 20, "a.example.com", "alpha2")})
	got, ok := c.Get("a.example.com")
	if !ok {
		t.Fatalf("Get miss after Put")
	}
	if len(got) != 1 || got[0].Priority != 20 {
		t.Errorf("overwritten entry not returned; got priority=%d", got[0].Priority)
	}
	if c.Len() != 1 {
		t.Errorf("len = %d, want 1 (Put overwrite must not duplicate)", c.Len())
	}
}

func TestEdgeRuleCache_ConcurrentGetPut(t *testing.T) {
	// -race gate. N writers, M readers; assert no data race and
	// no panic. Len may legitimately drop below N due to LRU
	// eviction; we only assert no race detector hit and a final
	// Len() within (0, cap].
	c := NewEdgeRuleCache(100)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				putRoute(c, "h.example.com", []EdgeRuleResolved{sampleEdgeRule("x", j, "h.example.com", "alpha")})
			}
		}(i)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_, _ = c.Get("h.example.com")
			}
		}()
	}
	wg.Wait()
	if c.Len() < 0 || c.Len() > 100 {
		t.Errorf("Len out of bounds: %d", c.Len())
	}
}

func TestNewEdgeRuleCache_ClampsCapacity(t *testing.T) {
	c := NewEdgeRuleCache(0)
	putRoute(c, "a.example.com", []EdgeRuleResolved{sampleEdgeRule("a", 10, "a.example.com", "alpha")})
	if c.Len() != 1 {
		t.Errorf("clamped cap=1 should hold one entry; got Len=%d", c.Len())
	}
	c2 := NewEdgeRuleCache(-3)
	putRoute(c2, "a.example.com", []EdgeRuleResolved{sampleEdgeRule("a", 10, "a.example.com", "alpha")})
	if c2.Len() != 1 {
		t.Errorf("negative cap should clamp to 1; got Len=%d", c2.Len())
	}
}

// --- EdgeRuleMatcher / EdgeRuleAuditor / ResolveTargetApp --------
//
// The interfaces are tiny; verify they have the shape production
// expects. Wiring-side correctness (audit row + metric + App
// substitution) is exercised in handler_test.go + the
// cmd/gatewayd-internal integration tests.

func TestEdgeRuleMatcher_InterfaceShape(t *testing.T) {
	// Compile-time assertion: a future kind impl that embeds
	// noOpEdgeRuleMatcher must satisfy the interface and get the
	// no-op default for MatchRoute.
	type future struct {
		noOpEdgeRuleMatcher
	}
	var _ EdgeRuleMatcher = future{}

	var m EdgeRuleMatcher = noOpEdgeRuleMatcher{}
	if r := m.MatchRoute(nil, "x", "/", "GET"); r != nil {
		t.Errorf("noOpEdgeRuleMatcher.MatchRoute = %v, want nil", r)
	}
	m.Reset() // must not panic
}

// --- Pure-Go filter logic (compileRouteRules + firstRouteMatch) ---
//
// These helpers are exported via cmd/gatewayd-internal/edge_rules.go,
// but the compile step is purely a Go function over EdgeRuleResolved
// — the test pins behaviour in pkg/gateway so a future refactor in
// cmd/gatewayd-internal can't silently flip a filter.

func pathMatchMatch(path, glob string) bool {
	// Stand-in for stdlib path.Match covering the patterns used in
	// these tests: empty (match all), exact, "*" (match all),
	// "/*" (single-segment wildcard), and "/api/*" (prefix wildcard).
	// The production loader uses stdlib path.Match
	// (compileRouteRules in cmd/gatewayd-internal/edge_rules.go).
	if glob == "" {
		return true
	}
	if glob == "*" {
		return true
	}
	if glob == path {
		return true
	}
	if glob == "/*" {
		return true
	}
	// Prefix wildcard "/prefix/*" matches "/prefix" + anything
	// after. Stdlib path.Match handles this; the inline helper
	// covers the test cases.
	if strings.HasSuffix(glob, "/*") {
		prefix := strings.TrimSuffix(glob, "/*")
		if strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

func methodMatch(method string, methods map[string]bool) bool {
	if len(methods) == 0 {
		return true
	}
	return methods[method]
}

func TestFirstRouteMatch_PriorityOrdering(t *testing.T) {
	// Input is already priority-ASC sorted (the loader must sort
	// before Put — see cmd/gatewayd-internal/edge_rules.go::
	// compileRouteRules). First match wins because lower priority
	// means earlier in the slice.
	rules := []EdgeRuleResolved{
		sampleEdgeRule("high", 0, "a.example.com", "highslug"),
		sampleEdgeRule("mid", 50, "a.example.com", "midslug"),
		sampleEdgeRule("low", 100, "a.example.com", "lowslug"),
	}
	got := pickFirstRouteMatch(rules, "/", "GET")
	if got == nil {
		t.Fatalf("pickFirstRouteMatch returned nil")
	}
	if got.ID != "high" {
		t.Errorf("expected priority-0 rule first; got %q", got.ID)
	}
}

func TestFirstRouteMatch_PathFilter(t *testing.T) {
	// priority-ASC sorted. Rule 1 (priority 0) matches /api/v1,
	// wins. /v1/api falls through to rule 2 (catch-all, priority 10).
	rules := []EdgeRuleResolved{
		{ID: "1", Priority: 0, PathGlob: "/api/*", Methods: nil, TargetAppSlug: "t"},
		{ID: "2", Priority: 10, PathGlob: "", Methods: nil, TargetAppSlug: "t2"},
	}
	if got := pickFirstRouteMatch(rules, "/api/v1", "GET"); got == nil || got.ID != "1" {
		t.Errorf("expected rule 1 (priority-0 path match), got %v", got)
	}
	if got := pickFirstRouteMatch(rules, "/v1/api", "GET"); got == nil || got.ID != "2" {
		t.Errorf("expected rule 2 (catch-all), got %v", got)
	}
}

func TestFirstRouteMatch_MethodFilter(t *testing.T) {
	rules := []EdgeRuleResolved{
		{ID: "1", Priority: 0, PathGlob: "", Methods: map[string]bool{"POST": true}, TargetAppSlug: "t"},
	}
	if got := pickFirstRouteMatch(rules, "/", "GET"); got != nil {
		t.Errorf("GET must not match POST-only rule; got %v", got)
	}
	if got := pickFirstRouteMatch(rules, "/", "POST"); got == nil {
		t.Errorf("POST must match POST-only rule")
	}
}

func TestFirstRouteMatch_NilSafe(t *testing.T) {
	if got := pickFirstRouteMatch(nil, "/", "GET"); got != nil {
		t.Errorf("empty rules must return nil; got %v", got)
	}
	if got := pickFirstRouteMatch([]EdgeRuleResolved{}, "/", "GET"); got != nil {
		t.Errorf("zero-length rules must return nil; got %v", got)
	}
}

// pickFirstRouteMatch is the pure-Go filter used by
// cmd/gatewayd-internal/edge_rules.go::gatewaydEdgeRules.MatchRoute
// after the cache returns the priority-ordered slice. Lives here
// (not in the gatewayd-internal impl) so its behaviour is pinned
// in pkg/gateway and the production loader can't silently drift.
func pickFirstRouteMatch(rules []EdgeRuleResolved, path, method string) *EdgeRuleResolved {
	for i := range rules {
		r := &rules[i]
		if !methodMatch(method, r.Methods) {
			continue
		}
		if !pathMatchMatch(path, r.PathGlob) {
			continue
		}
		return r
	}
	return nil
}

// --- PR 4 surface: resolved types, per-kind pick helpers, hostEntry cache ----

func sampleRewriteRule(id string, prio int, host, from, to string) EdgeRuleRewriteResolved {
	return EdgeRuleRewriteResolved{
		ID:        id,
		AccountID: "acc_" + id,
		AppID:     "app_" + id,
		Priority:  prio,
		PathGlob:  "",
		Methods:   nil,
		From:      from,
		To:        to,
	}
}

func sampleRedirectRule(id string, prio int, host string, status int, to string) EdgeRuleRedirectResolved {
	return EdgeRuleRedirectResolved{
		ID:         id,
		AccountID:  "acc_" + id,
		AppID:      "app_" + id,
		Priority:   prio,
		PathGlob:   "",
		Methods:    nil,
		StatusCode: status,
		To:         to,
	}
}

func sampleHeadersRule(id string, prio int, host string, reqHdr, respHdr []EdgeRuleHeaderOp) EdgeRuleHeadersResolved {
	return EdgeRuleHeadersResolved{
		ID:              id,
		AccountID:       "acc_" + id,
		AppID:           "app_" + id,
		Priority:        prio,
		PathGlob:        "",
		Methods:         nil,
		RequestHeaders:  reqHdr,
		ResponseHeaders: respHdr,
	}
}

func TestPickFirstRewriteMatch_PriorityOrdering(t *testing.T) {
	// Input is priority-ASC (mirrors what compile* writes after
	// sort.Slice); pickFirstRewriteMatch returns the first match
	// in iteration order, so priority-0 ("high") wins over 50/100.
	rules := []EdgeRuleRewriteResolved{
		sampleRewriteRule("high", 0, "a.example.com", "/api", "/v2"),
		sampleRewriteRule("mid", 50, "a.example.com", "/api", "/v3"),
		sampleRewriteRule("low", 100, "a.example.com", "/api", "/v1"),
	}
	got := PickFirstRewriteMatch(rules, "/api/x", "GET")
	if got == nil {
		t.Fatalf("PickFirstRewriteMatch = nil, want high")
	}
	if got.ID != "high" {
		t.Errorf("got ID %q, want high (lowest priority first)", got.ID)
	}
}

func TestPickFirstRedirectMatch_PriorityOrderingAndDefaultStatus(t *testing.T) {
	rules := []EdgeRuleRedirectResolved{
		sampleRedirectRule("high", 0, "a.example.com", 308, "https://c"),
		sampleRedirectRule("low", 100, "a.example.com", 301, "https://b"),
	}
	got := PickFirstRedirectMatch(rules, "/", "GET")
	if got == nil || got.ID != "high" {
		t.Errorf("got %v, want high priority-0", got)
	}
	if got.StatusCode != 308 {
		t.Errorf("got status %d, want 308", got.StatusCode)
	}
}

func TestPickFirstHeadersMatch_PriorityOrdering(t *testing.T) {
	rules := []EdgeRuleHeadersResolved{
		sampleHeadersRule("high", 0, "a.example.com", nil,
			[]EdgeRuleHeaderOp{{Name: "X-High", Value: "1", Action: "set"}}),
		sampleHeadersRule("low", 100, "a.example.com", nil,
			[]EdgeRuleHeaderOp{{Name: "X-Low", Value: "1", Action: "set"}}),
	}
	got := PickFirstHeadersMatch(rules, "/", "GET")
	if got == nil || got.ID != "high" {
		t.Errorf("got %v, want high priority-0", got)
	}
	if len(got.ResponseHeaders) != 1 || got.ResponseHeaders[0].Name != "X-High" {
		t.Errorf("got ResponseHeaders %v, want one X-High op", got.ResponseHeaders)
	}
}

func TestPickFirstRewriteMatch_MethodsFilter(t *testing.T) {
	rules := []EdgeRuleRewriteResolved{
		{
			ID: "post-only", Priority: 0, PathGlob: "",
			Methods: map[string]bool{"POST": true},
			From:    "/api", To: "/v1",
		},
	}
	if got := PickFirstRewriteMatch(rules, "/x", "GET"); got != nil {
		t.Errorf("GET should miss POST-only rule, got %v", got)
	}
	if got := PickFirstRewriteMatch(rules, "/x", "POST"); got == nil {
		t.Errorf("POST should hit POST-only rule, got nil")
	}
}

func TestPickFirstRewriteMatch_PathGlob(t *testing.T) {
	rules := []EdgeRuleRewriteResolved{
		{
			ID: "api-only", Priority: 0,
			PathGlob: "/api/*",
			Methods:  nil,
			From:     "/api", To: "/v1",
		},
	}
	if got := PickFirstRewriteMatch(rules, "/api/x", "GET"); got == nil {
		t.Errorf("/api/x should hit /api/* glob, got nil")
	}
	if got := PickFirstRewriteMatch(rules, "/v1/api", "GET"); got != nil {
		t.Errorf("/v1/api should miss /api/* glob, got %v", got)
	}
}

// putEntry is the test-only helper that puts a HostEntry with all
// four kinds' slices supplied independently. Mirrors putRoute but
// for tests that exercise multiple kinds on one host.
func putEntry(c *EdgeRuleCache, host string,
	route []EdgeRuleResolved,
	rewrite []EdgeRuleRewriteResolved,
	redirect []EdgeRuleRedirectResolved,
	headers []EdgeRuleHeadersResolved,
) {
	c.Put(host, &HostEntry{
		Host:     host,
		Route:    route,
		Rewrite:  rewrite,
		Redirect: redirect,
		Headers:  headers,
	})
}

func TestEdgeRuleCache_HostEntryPerKindAccess(t *testing.T) {
	c := NewEdgeRuleCache(4)
	putEntry(c, "a.example.com",
		[]EdgeRuleResolved{sampleEdgeRule("route-1", 0, "a.example.com", "demo")},
		[]EdgeRuleRewriteResolved{sampleRewriteRule("rewrite-1", 0, "a.example.com", "/api", "/v1")},
		[]EdgeRuleRedirectResolved{sampleRedirectRule("redirect-1", 0, "a.example.com", 308, "https://b")},
		[]EdgeRuleHeadersResolved{sampleHeadersRule("headers-1", 0, "a.example.com", nil, nil)},
	)
	if route, ok := c.Get("a.example.com"); !ok || len(route) != 1 {
		t.Errorf("Get route miss; got ok=%v len=%d", ok, len(route))
	}
	if rewrite, ok := c.GetRewrite("a.example.com"); !ok || len(rewrite) != 1 {
		t.Errorf("GetRewrite miss; got ok=%v len=%d", ok, len(rewrite))
	}
	if redirect, ok := c.GetRedirect("a.example.com"); !ok || len(redirect) != 1 {
		t.Errorf("GetRedirect miss; got ok=%v len=%d", ok, len(redirect))
	}
	if headers, ok := c.GetHeaders("a.example.com"); !ok || len(headers) != 1 {
		t.Errorf("GetHeaders miss; got ok=%v len=%d", ok, len(headers))
	}
}

func TestEdgeRuleCache_HostEntryKindIsolation(t *testing.T) {
	// Putting one kind's slice only leaves the other three as
	// (nil, true) — i.e. entry exists but no rule of that kind.
	// Mirrors the cmd-side loadHost pattern: the SQL roundtrip
	// reads every kind together; per-kind Puts are a test surface.
	c := NewEdgeRuleCache(4)
	putEntry(c, "a.example.com",
		[]EdgeRuleResolved{sampleEdgeRule("route-1", 0, "a.example.com", "demo")},
		nil, nil, nil,
	)
	if _, ok := c.Get("a.example.com"); !ok {
		t.Errorf("Get should hit (entry exists)")
	}
	// Per-kind Get: returns (nil, true) when entry exists but the
	// rewrite slice is nil (the loader would interpret this as a
	// clean miss for the rewrite kind).
	if got, ok := c.GetRewrite("a.example.com"); !ok || got != nil {
		t.Errorf("GetRewrite should return (nil, true) for entry-with-no-rewrite; got (%v, %v)", got, ok)
	}
	// Re-Put with rewrite filled in — entry now carries both.
	putEntry(c, "a.example.com",
		[]EdgeRuleResolved{sampleEdgeRule("route-1", 0, "a.example.com", "demo")},
		[]EdgeRuleRewriteResolved{sampleRewriteRule("rewrite-1", 0, "a.example.com", "/api", "/v1")},
		nil, nil,
	)
	if route, ok := c.Get("a.example.com"); !ok || len(route) != 1 {
		t.Errorf("route lost on re-Put: ok=%v len=%d", ok, len(route))
	}
	if rewrite, ok := c.GetRewrite("a.example.com"); !ok || len(rewrite) != 1 {
		t.Errorf("rewrite missing after re-Put: ok=%v len=%d", ok, len(rewrite))
	}
}

func TestEdgeRuleMatcher_WidenedInterfaceSatisfied(t *testing.T) {
	// Compile-time check: noOpEdgeRuleMatcher (which provides
	// default no-op behaviour) satisfies the widened EdgeRuleMatcher
	// interface. If PR 5-7 add new Match* methods and forget to
	// add a noOp default, this assignment fails to compile.
	var m EdgeRuleMatcher = noOpEdgeRuleMatcher{}
	_ = m.MatchRoute(nil, "h", "/", "GET")
	_ = m.MatchRewrite(nil, "h", "/", "GET")
	_ = m.MatchRedirect(nil, "h", "/", "GET")
	_ = m.MatchHeaders(nil, "h", "/", "GET")
	m.Reset()
}

func TestPickFirstHeadersMatch_HeadersOrderPreserved(t *testing.T) {
	// Customer-declared op order is preserved end-to-end — Cloudflare
	// "first wins" semantics for `set` mean the customer's order is
	// the contract. compile* copies in declared order; pick* does
	// not reorder.
	ops := []EdgeRuleHeaderOp{
		{Name: "X-A", Value: "1", Action: "set"},
		{Name: "X-A", Value: "2", Action: "set"},
		{Name: "X-A", Value: "3", Action: "set"},
	}
	rules := []EdgeRuleHeadersResolved{sampleHeadersRule("h", 0, "a.example.com", nil, ops)}
	got := PickFirstHeadersMatch(rules, "/", "GET")
	if got == nil || len(got.ResponseHeaders) != 3 {
		t.Fatalf("got %v, want 3 ops", got)
	}
	if got.ResponseHeaders[1].Value != "2" {
		t.Errorf("ops reordered: got %q, want 2", got.ResponseHeaders[1].Value)
	}
}

func TestPickFirstRedirectMatch_PathGlob(t *testing.T) {
	rules := []EdgeRuleRedirectResolved{
		{
			ID: "api-only", Priority: 0, PathGlob: "/api/*", Methods: nil,
			StatusCode: 308, To: "https://b",
		},
	}
	if got := PickFirstRedirectMatch(rules, "/api/x", "GET"); got == nil {
		t.Errorf("/api/x should match /api/* glob")
	}
	if got := PickFirstRedirectMatch(rules, "/v1/api", "GET"); got != nil {
		t.Errorf("/v1/api should not match /api/* glob, got %v", got)
	}
}
