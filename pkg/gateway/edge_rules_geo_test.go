// Tests for the kind=geo edge-rule plumbing (ADR-091 D21).
// Mirrors the existing kind=ip tests in edge_rules_test.go:
// exercise the cache insertion + the pickFirst filter, and
// confirm the matcher interface contract is satisfied by
// (a) the noOpEdgeRuleMatcher (already covered by the
// `var _ EdgeRuleMatcher = (*noOpEdgeRuleMatcher)(nil)` check
// in edge_rules.go's compile-time guard) and (b) a fresh
// local stub that returns a geo rule.
package gateway

import (
	"testing"
)

// sampleGeoRule is a small constructor for the table-driven
// tests below. Mirrors sampleIPRule in shape.
func sampleGeoRule(id string, prio int, allow, deny []string, pathGlob string) EdgeRuleGeoResolved {
	allowSet := make(map[string]struct{}, len(allow))
	for _, c := range allow {
		allowSet[c] = struct{}{}
	}
	denySet := make(map[string]struct{}, len(deny))
	for _, c := range deny {
		denySet[c] = struct{}{}
	}
	return EdgeRuleGeoResolved{
		ID:        id,
		AccountID: "acct-test",
		AppID:     "app-test",
		Priority:  prio,
		PathGlob:  pathGlob,
		Methods:   nil, // nil = match any method
		Allow:     allowSet,
		Deny:      denySet,
	}
}

// TestPickFirstGeoMatch_PriorityOrdering: the priority-ASC
// filter (lower number wins) is the load-bearing detail in
// the §4.1.2.8b ordering. A regression here swaps the rule
// that fires and the customer sees "wrong" rules activating.
// Input is pre-sorted (compile-side responsibility — see
// compileGeoRules sort.Slice at cmd/gatewayd-internal/edge_rules.go).
func TestPickFirstGeoMatch_PriorityOrdering(t *testing.T) {
	rules := []EdgeRuleGeoResolved{
		sampleGeoRule("high", 0, []string{"DE"}, nil, ""),
		sampleGeoRule("low", 100, []string{"FR"}, nil, ""),
	}
	got := PickFirstGeoMatch(rules, "/", "GET")
	if got == nil || got.ID != "high" {
		t.Errorf("PickFirstGeoMatch priority = %v, want ID=high", got)
	}
}

// TestPickFirstGeoMatch_PathFilter: the path-glob filter
// must skip a rule whose glob doesn't match. Mirrors the
// path-filter test for kind=ip. Input is pre-sorted by priority
// (compileGeoRules at cmd/gatewayd-internal/edge_rules.go owns
// the sort). Within a single priority tier the first declaration
// wins — the matcher short-circuits on the first hit, which is
// the load-bearing §4.1.2.8b ordering contract.
func TestPickFirstGeoMatch_PathFilter(t *testing.T) {
	// Two rules with distinct priorities: rule a (prio=0, no glob)
	// always wins when path is /health; rule a also wins on /api/v1
	// because priority dominates over path-glob (the contract is
	// "lowest priority wins, first declaration tiebreaks").
	rules := []EdgeRuleGeoResolved{
		sampleGeoRule("a", 0, []string{"DE"}, nil, ""),
		sampleGeoRule("b", 10, []string{"FR"}, nil, "/api/*"),
	}
	got := PickFirstGeoMatch(rules, "/health", "GET")
	if got == nil || got.ID != "a" {
		t.Errorf("PickFirstGeoMatch(/health) = %v, want ID=a", got)
	}
	got = PickFirstGeoMatch(rules, "/api/v1", "GET")
	if got == nil || got.ID != "a" {
		t.Errorf("PickFirstGeoMatch(/api/v1) = %v, want ID=a (lower prio wins regardless of path glob)", got)
	}
	// Path-glob filter takes effect when b is at the LOWER priority:
	// the matcher tries b first; /api/v1 matches its glob → return b;
	// /health doesn't match b's glob → fall through to a.
	alt := []EdgeRuleGeoResolved{
		sampleGeoRule("b", 0, []string{"FR"}, nil, "/api/*"),
		sampleGeoRule("a", 10, []string{"DE"}, nil, ""),
	}
	got = PickFirstGeoMatch(alt, "/api/v1", "GET")
	if got == nil || got.ID != "b" {
		t.Errorf("PickFirstGeoMatch(/api/v1, alt) = %v, want ID=b", got)
	}
	got = PickFirstGeoMatch(alt, "/health", "GET")
	if got == nil || got.ID != "a" {
		t.Errorf("PickFirstGeoMatch(/health, alt) = %v, want ID=a (b's /api/* glob doesn't match /health, falls through)", got)
	}
}

// TestPickFirstGeoMatch_MethodFilter: a rule with a non-empty
// Methods map only matches the listed methods. Mirrors the
// method-filter test for kind=ip.
func TestPickFirstGeoMatch_MethodFilter(t *testing.T) {
	r := sampleGeoRule("a", 0, []string{"DE"}, nil, "")
	r.Methods = map[string]bool{"GET": true, "POST": true}
	rules := []EdgeRuleGeoResolved{r}
	if got := PickFirstGeoMatch(rules, "/", "DELETE"); got != nil {
		t.Errorf("PickFirstGeoMatch DELETE = %v, want nil", got)
	}
	if got := PickFirstGeoMatch(rules, "/", "GET"); got == nil {
		t.Errorf("PickFirstGeoMatch GET = nil, want hit")
	}
}

// TestPickFirstGeoMatch_NilSafe: an empty slice returns nil.
// Mirrors the nil-safe test for kind=ip.
func TestPickFirstGeoMatch_NilSafe(t *testing.T) {
	if got := PickFirstGeoMatch(nil, "/", "GET"); got != nil {
		t.Errorf("PickFirstGeoMatch(nil) = %v, want nil", got)
	}
	if got := PickFirstGeoMatch([]EdgeRuleGeoResolved{}, "/", "GET"); got != nil {
		t.Errorf("PickFirstGeoMatch([]) = %v, want nil", got)
	}
}

// TestCache_PutAndGetGeo: the cache round-trips the kind=geo
// slice via the GetGeo accessor. Mirrors the IP equivalent.
func TestCache_PutAndGetGeo(t *testing.T) {
	c := NewEdgeRuleCache(4)
	rules := []EdgeRuleGeoResolved{
		sampleGeoRule("a", 10, []string{"DE"}, nil, ""),
	}
	c.Put("h.example.com", &HostEntry{Geo: rules})
	got, ok := c.GetGeo("h.example.com")
	if !ok {
		t.Fatalf("GetGeo = not ok, want hit")
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Errorf("GetGeo = %+v, want [a]", got)
	}
}

// TestCache_GetGeo_MissReturnsFalse: a host that was not Put
// returns (nil, false). Mirrors the IP miss semantics.
func TestCache_GetGeo_MissReturnsFalse(t *testing.T) {
	c := NewEdgeRuleCache(4)
	if _, ok := c.GetGeo("never.put"); ok {
		t.Errorf("GetGeo(never.put) = ok, want miss")
	}
}

// TestCache_GetGeo_DeepCopiesInnerMaps pins the deep-copy contract
// introduced after PR #845 review finding #2: GetGeo must clone the
// per-entry Methods/Allow/Deny maps, not share them with the cache
// entry. A caller that mutates the returned struct's maps (e.g.
// for a hypothetical "consume" pattern where a matched country is
// removed from the in-memory set to short-circuit subsequent
// matches) must NOT poison the cache. Mirrors the sibling
// TestCache_GetIP_DeepCopiesInnerMaps in edge_rules_test.go.
func TestCache_GetGeo_DeepCopiesInnerMaps(t *testing.T) {
	c := NewEdgeRuleCache(4)
	r := sampleGeoRule("a", 0, []string{"DE", "FR"}, []string{"US"}, "")
	r.Methods = map[string]bool{"GET": true}
	c.Put("h.example.com", &HostEntry{Geo: []EdgeRuleGeoResolved{r}})

	// First Get — mutate the maps on the returned copy.
	got1, ok := c.GetGeo("h.example.com")
	if !ok || len(got1) != 1 {
		t.Fatalf("GetGeo#1 = %+v ok=%v, want one entry", got1, ok)
	}
	delete(got1[0].Allow, "DE")
	got1[0].Methods["POST"] = true
	delete(got1[0].Deny, "US")

	// Second Get — the cache must still expose the original keys.
	got2, ok := c.GetGeo("h.example.com")
	if !ok || len(got2) != 1 {
		t.Fatalf("GetGeo#2 = %+v ok=%v, want one entry", got2, ok)
	}
	if _, ok := got2[0].Allow["DE"]; !ok {
		t.Errorf("GetGeo#2 Allow is missing DE; cache was poisoned by Get#1 mutation")
	}
	if _, ok := got2[0].Allow["FR"]; !ok {
		t.Errorf("GetGeo#2 Allow is missing FR; cache was poisoned by Get#1 mutation")
	}
	if _, ok := got2[0].Deny["US"]; !ok {
		t.Errorf("GetGeo#2 Deny is missing US; cache was poisoned by Get#1 mutation")
	}
	if got2[0].Methods["POST"] {
		t.Errorf("GetGeo#2 Methods contains POST; cache was poisoned by Get#1 mutation")
	}
}
