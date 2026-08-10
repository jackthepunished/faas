// Tests for cmd/gatewayd-internal/edge_rules.go (PR 3 of the Edge
// Rules rollout, ADR-089 / issue #561). Pins the production wiring:
//   - compileRouteRules filters store rules to kind=route,
//     compiles methods into a lookup map, and sorts priority-ASC.
//   - gatewaydEdgeRules.MatchRoute goes cache → store (on miss) →
//     compile → Put → PickFirstRouteMatch.
//   - gatewaydEdgeRules.Reset drops the cache.
//   - gatewaydEdgeRulesAud.Emit is a thin wrapper over *gatewaydAuditor.
//
// Mirrors cmd/gatewayd-internal/backend_test.go shape (small fakeStore
// + sync.Mutex for race). The pg-backed integration (live invalidation
// via pg_notify, cross-account blocked) lives in pkg/state tests; this
// file stays cmd-side only.

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/onebox-faas/faas/pkg/state"
)

// fakeEdgeRuleStore is the test double for edgeRuleStore. It returns
// canned rule slices keyed by host; an optional `err` returns a
// synthetic error so loader failures are exercised.
type fakeEdgeRuleStore struct {
	mu    sync.Mutex
	rules map[string][]state.EdgeRule
	err   error
	calls map[string]int // host -> call count, for loader-frequency assertions
}

func (f *fakeEdgeRuleStore) MatchEdgeRulesForHost(_ context.Context, host string) ([]state.EdgeRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[host]++
	return f.rules[host], nil
}

// sampleRouteRule builds a fully-populated state.EdgeRule with the
// given id/priority/host/path/methods and a kind=route action whose
// TargetAppSlug is the supplied slug.
func sampleRouteRule(id string, priority int, host, path string, methods []string, targetSlug string) state.EdgeRule {
	return state.EdgeRule{
		ID:           id,
		AccountID:    "acc_test",
		AppID:        "app_test",
		MatchHost:    host,
		MatchPath:    path,
		MatchMethods: methods,
		Priority:     priority,
		Enabled:      true,
		Kind:         state.EdgeRuleKindRoute,
		Action: state.EdgeRuleAction{
			Kind:  state.EdgeRuleKindRoute,
			Route: &state.EdgeRuleRouteAction{TargetAppSlug: targetSlug},
		},
	}
}

// --- compileRouteRules ---------------------------------------------------

func TestCompileRouteRules_KeepsOnlyKindRoute(t *testing.T) {
	// PR 3 loader filters out non-route kinds. PR 4-7 widen the
	// filter to compile their own kinds; this test pins PR 3.
	in := []state.EdgeRule{
		sampleRouteRule("route-1", 100, "a.example.com", "/", nil, "demo"),
		{
			ID: "rewrite-1", AccountID: "acc_test", AppID: "app_test",
			MatchHost: "a.example.com", Priority: 0, Enabled: true,
			Kind: state.EdgeRuleKindRewrite,
			Action: state.EdgeRuleAction{
				Kind:    state.EdgeRuleKindRewrite,
				Rewrite: &state.EdgeRuleRewriteAction{From: "/api", To: "/v1"},
			},
		},
	}
	got := compileRouteRules(in)
	if len(got) != 1 {
		t.Fatalf("got %d rules, want 1 (kind filter)", len(got))
	}
	if got[0].ID != "route-1" {
		t.Errorf("got %q, want route-1", got[0].ID)
	}
}

func TestCompileRouteRules_SkipsDisabled(t *testing.T) {
	// Disabled rules must NEVER steer traffic. apid toggles
	// Enable=false to deactivate a rule without losing the row.
	in := []state.EdgeRule{
		sampleRouteRule("route-on", 100, "a.example.com", "/", nil, "demo"),
		func() state.EdgeRule {
			r := sampleRouteRule("route-off", 0, "a.example.com", "/", nil, "demo")
			r.Enabled = false
			return r
		}(),
	}
	got := compileRouteRules(in)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1 (disabled filtered)", len(got))
	}
	if got[0].ID != "route-on" {
		t.Errorf("got %q, want route-on (priority 0 disabled rule dropped)", got[0].ID)
	}
}

func TestCompileRouteRules_SortsPriorityAscending(t *testing.T) {
	// Loader MUST sort before Put so PickFirstRouteMatch returns
	// the lowest-priority (earliest) match. The store returns
	// rows in arbitrary order; the compile step is the only
	// place this invariant lives.
	in := []state.EdgeRule{
		sampleRouteRule("low", 100, "a.example.com", "/", nil, "demo"),
		sampleRouteRule("high", 0, "a.example.com", "/", nil, "demo"),
		sampleRouteRule("mid", 50, "a.example.com", "/", nil, "demo"),
	}
	got := compileRouteRules(in)
	if len(got) != 3 {
		t.Fatalf("got %d, want 3", len(got))
	}
	if got[0].Priority != 0 || got[1].Priority != 50 || got[2].Priority != 100 {
		t.Errorf("priority order = [%d %d %d], want [0 50 100]",
			got[0].Priority, got[1].Priority, got[2].Priority)
	}
}

func TestCompileRouteRules_CompilesMethodsLookupTable(t *testing.T) {
	// The per-request methods filter is O(1) via the map; the
	// loader is the only place the slice → map conversion happens.
	in := []state.EdgeRule{
		sampleRouteRule("r", 0, "a.example.com", "/", []string{"get", "POST"}, "demo"),
	}
	got := compileRouteRules(in)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	if !got[0].Methods["GET"] {
		t.Errorf("GET not in methods (must be upper-cased)")
	}
	if !got[0].Methods["POST"] {
		t.Errorf("POST not in methods")
	}
	if got[0].Methods["DELETE"] {
		t.Errorf("DELETE in methods, must NOT be present")
	}
}

func TestCompileRouteRules_EmptyInputProducesEmptyOutput(t *testing.T) {
	// Edge case: store returns no rules → Put is a no-op (the
	// gateway cache drops empty slices); the loader must not
	// allocate a zero-length slice header that gets Put.
	if got := compileRouteRules(nil); got != nil {
		t.Errorf("compileRouteRules(nil) = %v, want nil", got)
	}
	if got := compileRouteRules([]state.EdgeRule{}); got != nil {
		t.Errorf("compileRouteRules([]) = %v, want nil", got)
	}
}

// --- gatewaydEdgeRules (cache + loader) ---------------------------------

func newQuietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestGatewaydEdgeRules_MatchRoute_CacheMissHitsStore(t *testing.T) {
	store := &fakeEdgeRuleStore{
		rules: map[string][]state.EdgeRule{
			"a.example.com": {
				sampleRouteRule("route-1", 100, "a.example.com", "/", nil, "demo"),
			},
		},
	}
	g := newGatewaydEdgeRules(store, newQuietLogger())
	got := g.MatchRoute(context.Background(), "a.example.com", "/", "GET")
	if got == nil {
		t.Fatalf("MatchRoute = nil, want rule")
	}
	if got.ID != "route-1" {
		t.Errorf("got %q, want route-1", got.ID)
	}
	if got.TargetAppSlug != "demo" {
		t.Errorf("got target %q, want demo", got.TargetAppSlug)
	}
	if store.calls["a.example.com"] != 1 {
		t.Errorf("store calls = %d, want 1", store.calls["a.example.com"])
	}
}

func TestGatewaydEdgeRules_MatchRoute_CacheHitSkipsStore(t *testing.T) {
	// Second MatchRoute on the same host must NOT re-hit the
	// store — that's the whole point of the LRU. Pin the
	// invariant so a future refactor (e.g. eagerly revalidating
	// on every request) doesn't silently widen the hit window.
	store := &fakeEdgeRuleStore{
		rules: map[string][]state.EdgeRule{
			"a.example.com": {
				sampleRouteRule("route-1", 100, "a.example.com", "/", nil, "demo"),
			},
		},
	}
	g := newGatewaydEdgeRules(store, newQuietLogger())
	for i := 0; i < 5; i++ {
		_ = g.MatchRoute(context.Background(), "a.example.com", "/", "GET")
	}
	if store.calls["a.example.com"] != 1 {
		t.Errorf("store calls = %d, want 1 (cache hit should skip store)", store.calls["a.example.com"])
	}
}

func TestGatewaydEdgeRules_MatchRoute_StoreErrorIsMiss(t *testing.T) {
	// A loader failure is a transient miss — return nil and
	// log. The handler then falls through to Backend.Lookup;
	// the customer sees a 404, not a 500. (A future PR could
	// add a last-error gauge; PR 3 stays silent.)
	store := &fakeEdgeRuleStore{err: errors.New("pg unreachable")}
	g := newGatewaydEdgeRules(store, newQuietLogger())
	if got := g.MatchRoute(context.Background(), "a.example.com", "/", "GET"); got != nil {
		t.Errorf("MatchRoute on store error = %v, want nil", got)
	}
}

func TestGatewaydEdgeRules_ResetDropsCache(t *testing.T) {
	store := &fakeEdgeRuleStore{
		rules: map[string][]state.EdgeRule{
			"a.example.com": {
				sampleRouteRule("route-1", 100, "a.example.com", "/", nil, "demo"),
			},
		},
	}
	g := newGatewaydEdgeRules(store, newQuietLogger())
	_ = g.MatchRoute(context.Background(), "a.example.com", "/", "GET")
	if g.cache.Len() != 1 {
		t.Fatalf("len before Reset = %d, want 1", g.cache.Len())
	}
	g.Reset()
	if g.cache.Len() != 0 {
		t.Errorf("len after Reset = %d, want 0", g.cache.Len())
	}
	if store.calls["a.example.com"] != 1 {
		t.Errorf("Reset called = %d, want 1 (Reset must not re-hit store)", store.calls["a.example.com"])
	}
	// Next MatchRoute re-hits store.
	_ = g.MatchRoute(context.Background(), "a.example.com", "/", "GET")
	if store.calls["a.example.com"] != 2 {
		t.Errorf("store calls post-Reset = %d, want 2", store.calls["a.example.com"])
	}
}

// --- gatewaydEdgeRulesAud (audit thin wrapper) ---------------------------

func TestGatewaydEdgeRulesAud_NilReceiverIsNoOp(t *testing.T) {
	// PR 3 production wires a real *gatewaydAuditor; unit tests
	// that don't exercise the audit path can pass nil. The
	// receiver is a pointer so a nil receiver must NOT panic.
	var aud *gatewaydEdgeRulesAud
	aud.Emit(context.Background(), "edge_rule.route_matched", nil, map[string]any{
		"rule_id": "r1",
	}) // must not panic
}

func TestGatewaydEdgeRulesAud_NilInnerIsNoOp(t *testing.T) {
	// Defence in depth: a wrapper whose inner auditor is nil
	// must drop the row instead of panicking. Same shape as
	// gatewaydAuditor.Emit's nil-self guard.
	aud := &gatewaydEdgeRulesAud{inner: nil}
	aud.Emit(context.Background(), "edge_rule.route_matched", nil, map[string]any{}) // must not panic
}
