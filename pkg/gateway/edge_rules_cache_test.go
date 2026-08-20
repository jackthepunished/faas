package gateway

import (
	"testing"

	"github.com/onebox-faas/faas/pkg/state"
)

// sampleCacheRule returns a well-formed EdgeRuleCacheResolved
// for table-driven tests. i disambiguates the slice element
// (priority varies); mutate via the test body.
func sampleCacheRule(id string, prio int, host string) EdgeRuleCacheResolved {
	return EdgeRuleCacheResolved{
		ID:                  id,
		AccountID:           "acct-" + id,
		AppID:               "app-" + id,
		Priority:            prio,
		PathGlob:            "",
		Methods:             map[string]bool{"GET": true, "HEAD": true},
		DeploymentID:        "dep-" + id,
		MaxAgeSeconds:       60,
		StaleIfErrorSeconds: 300,
		VaryOn:              []string{"Accept-Language"},
	}
}

// TestPickFirstCacheMatch_HappyPath pins the priority-ASC +
// methods + path-glob filter. A rule with prio=10 beats a
// rule with prio=20 (lower priority value = higher precedence,
// matching the row's CHECK ordering).
func TestPickFirstCacheMatch_HappyPath(t *testing.T) {
	rules := []EdgeRuleCacheResolved{
		sampleCacheRule("bg", 10, "shop.example.com"),
		sampleCacheRule("bg", 20, "shop.example.com"),
	}
	got := PickFirstCacheMatch(rules, "/catalog/foo", "GET")
	if got == nil {
		t.Fatal("expected a match, got nil")
	}
	if got.ID != "bg" || got.Priority != 10 {
		t.Errorf("match = %+v, want ID=bg Priority=10", got)
	}
}

// TestPickFirstCacheMatch_MethodFilter pins that a non-matching
// method is rejected and the matcher walks to the next rule.
func TestPickFirstCacheMatch_MethodFilter(t *testing.T) {
	rules := []EdgeRuleCacheResolved{
		sampleCacheRule("get-only", 10, "shop.example.com"),
		sampleCacheRule("both", 20, "shop.example.com"),
	}
	rules[0].Methods = map[string]bool{"GET": true}
	got := PickFirstCacheMatch(rules, "/catalog/foo", "HEAD")
	if got == nil {
		t.Fatal("expected a match, got nil")
	}
	if got.ID != "both" {
		t.Errorf("match = %s, want both (the GET-only rule must be skipped on a HEAD request)", got.ID)
	}
}

// TestPickFirstCacheMatch_PathGlob pins the path-glob filter:
// "/catalog/*" matches "/catalog/foo" but not "/products/foo".
func TestPickFirstCacheMatch_PathGlob(t *testing.T) {
	rules := []EdgeRuleCacheResolved{
		sampleCacheRule("catalog", 10, "shop.example.com"),
	}
	rules[0].PathGlob = "/catalog/*"
	if got := PickFirstCacheMatch(rules, "/catalog/foo", "GET"); got == nil {
		t.Error("expected a match for /catalog/foo")
	}
	if got := PickFirstCacheMatch(rules, "/products/foo", "GET"); got != nil {
		t.Errorf("expected no match for /products/foo, got %+v", got)
	}
}

// TestPickFirstCacheMatch_NoMatch pins the empty-slice + nil
// cases. A nil rules slice must return nil; an empty slice
// must return nil; a slice whose first entry has neither
// matching method nor matching path must return nil.
func TestPickFirstCacheMatch_NoMatch(t *testing.T) {
	if got := PickFirstCacheMatch(nil, "/x", "GET"); got != nil {
		t.Errorf("nil slice: got %+v, want nil", got)
	}
	if got := PickFirstCacheMatch([]EdgeRuleCacheResolved{}, "/x", "GET"); got != nil {
		t.Errorf("empty slice: got %+v, want nil", got)
	}
	rules := []EdgeRuleCacheResolved{sampleCacheRule("only-post", 10, "shop.example.com")}
	rules[0].Methods = map[string]bool{"POST": true}
	if got := PickFirstCacheMatch(rules, "/x", "GET"); got != nil {
		t.Errorf("non-matching method: got %+v, want nil", got)
	}
}

// TestEdgeRuleCacheResolved_ToStateEdgeRuleCacheAction pins
// the adapter that feeds the runtime cache store. The
// returned state.EdgeRuleCacheAction MUST be a fresh pointer
// (defensive copy of VaryOn) so the store's lifetime does not
// alias the resolved slice.
func TestEdgeRuleCacheResolved_ToStateEdgeRuleCacheAction(t *testing.T) {
	r := sampleCacheRule("r", 10, "shop.example.com")
	a := r.toStateEdgeRuleCacheAction()
	if a == nil {
		t.Fatal("adapter returned nil for a non-nil rule")
	}
	if a.MaxAgeSeconds != 60 {
		t.Errorf("MaxAgeSeconds = %d, want 60", a.MaxAgeSeconds)
	}
	if a.StaleIfErrorSeconds != 300 {
		t.Errorf("StaleIfErrorSeconds = %d, want 300", a.StaleIfErrorSeconds)
	}
	if len(a.VaryOn) != 1 || a.VaryOn[0] != "Accept-Language" {
		t.Errorf("VaryOn = %v, want [Accept-Language]", a.VaryOn)
	}
	// Nil receiver must not panic.
	var nilR *EdgeRuleCacheResolved
	if a := nilR.toStateEdgeRuleCacheAction(); a != nil {
		t.Errorf("nil receiver: got %+v, want nil", a)
	}
	// Defensive copy: mutating the returned VaryOn must not
	// alias the resolved slice.
	a.VaryOn[0] = "Mutated"
	if r.VaryOn[0] == "Mutated" {
		t.Error("VaryOn is shared between the adapter return and the resolved slice (defensive copy required)")
	}
}

// TestEdgeRuleCacheResolved_ToStateEdgeRuleCacheAction_EmptyVaryOn
// pins the empty-VaryOn path. The runtime cache key is then
// "URL only" — no header-based fan-out. Pinning this means a
// future refactor that treats an empty VaryOn as "all headers"
// would break the test.
func TestEdgeRuleCacheResolved_ToStateEdgeRuleCacheAction_EmptyVaryOn(t *testing.T) {
	r := sampleCacheRule("r", 10, "shop.example.com")
	r.VaryOn = nil
	a := r.toStateEdgeRuleCacheAction()
	if len(a.VaryOn) != 0 {
		t.Errorf("VaryOn len = %d, want 0", len(a.VaryOn))
	}
}

// _ keeps the state package's referenced type alive for the
// unused-import linter when this file is built in isolation.
var _ = state.EdgeRuleKindCache
