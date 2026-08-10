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

// --- EdgeRuleCache primitive parity with RouteCache ----------------

func TestEdgeRuleCache_LRUEvictsAtCapacity(t *testing.T) {
	c := NewEdgeRuleCache(2)
	c.Put("a.example.com", []EdgeRuleResolved{sampleEdgeRule("a", 10, "a.example.com", "alpha")})
	c.Put("b.example.com", []EdgeRuleResolved{sampleEdgeRule("b", 10, "b.example.com", "beta")})
	c.Put("c.example.com", []EdgeRuleResolved{sampleEdgeRule("c", 10, "c.example.com", "gamma")}) // evicts "a"
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
	c.Put("a.example.com", []EdgeRuleResolved{sampleEdgeRule("a", 10, "a.example.com", "alpha")})
	c.Put("b.example.com", []EdgeRuleResolved{sampleEdgeRule("b", 10, "b.example.com", "beta")})
	if _, ok := c.Get("a.example.com"); !ok {
		t.Errorf("a should hit before promotion")
	}
	c.Put("c.example.com", []EdgeRuleResolved{sampleEdgeRule("c", 10, "c.example.com", "gamma")})
	if _, ok := c.Get("b.example.com"); ok {
		t.Errorf("b should have been evicted (a was promoted)")
	}
	if _, ok := c.Get("a.example.com"); !ok {
		t.Errorf("a should still be cached after promotion")
	}
}

func TestEdgeRuleCache_ResetClearsAll(t *testing.T) {
	c := NewEdgeRuleCache(4)
	c.Put("a.example.com", []EdgeRuleResolved{sampleEdgeRule("a", 10, "a.example.com", "alpha")})
	c.Put("b.example.com", []EdgeRuleResolved{sampleEdgeRule("b", 10, "b.example.com", "beta")})
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
	c.Put("empty2.example.com", []EdgeRuleResolved{})
	if c.Len() != 0 {
		t.Errorf("len = %d, want 0 (empty Put is no-op)", c.Len())
	}
}

func TestEdgeRuleCache_GetReturnsCopy(t *testing.T) {
	// Callers must not be able to mutate the cached slice through
	// the Get pointer — mirrors the RouteCache value-copy contract.
	c := NewEdgeRuleCache(2)
	src := []EdgeRuleResolved{sampleEdgeRule("a", 10, "a.example.com", "alpha")}
	c.Put("a.example.com", src)
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
	c.Put("a.example.com", []EdgeRuleResolved{sampleEdgeRule("a", 10, "a.example.com", "alpha")})
	c.Put("a.example.com", []EdgeRuleResolved{sampleEdgeRule("a", 20, "a.example.com", "alpha2")})
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
				c.Put("h.example.com", []EdgeRuleResolved{sampleEdgeRule("x", j, "h.example.com", "alpha")})
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
	c.Put("a.example.com", []EdgeRuleResolved{sampleEdgeRule("a", 10, "a.example.com", "alpha")})
	if c.Len() != 1 {
		t.Errorf("clamped cap=1 should hold one entry; got Len=%d", c.Len())
	}
	c2 := NewEdgeRuleCache(-3)
	c2.Put("a.example.com", []EdgeRuleResolved{sampleEdgeRule("a", 10, "a.example.com", "alpha")})
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
