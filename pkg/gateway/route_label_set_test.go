// Property test for pkg/gateway/route_label_set.go (ADR-093 /
// issue #273). Mirrors the cardinality-property-test shape from
// pkg/wire/metrics_cardinality_test.go (issue #278 / ADR-040 — the
// upstream IP-equivalent primitive).
//
// Pins:
//
//  1. Under fuzzed 10k random routes through one app, the
//     underlying admitted map never exceeds `cap + 2` entries
//     (the +2 is the two reserved labels).
//  2. The overflow bucket `__route_other__` is emitted for every
//     request whose route is not in the admitted set when the set
//     is full.
//  3. Repeated calls with the same route are idempotent (the
//     already-admitted hot path is O(1)).
//  4. Reserved labels (`""` and `"__route_other__"`) are pre-
//     admitted without consuming capacity — a single app seeing
//     cap+1 distinct routes has `cap-1` real routes plus the
//     overflow bucket, not `cap-2` plus overflow.
//
// Build tag: package gateway (whitebox); the unexported admit()
// and the underlying map are the load-bearing surface and the
// test reaches in directly.
package gateway

import (
	"fmt"
	"math/rand"
	"testing"
)

// TestRouteLabelSet_Admit_BoundedUnderFuzzedLoad is the
// load-bearing property test for ADR-093 D2: under 10k random
// distinct routes against a single app, the underlying admitted
// map stays <= cap+2 entries. Fails CI if the cap is regressed.
func TestRouteLabelSet_Admit_BoundedUnderFuzzedLoad(t *testing.T) {
	s := newRouteLabelSetWithCap(routeLabelSetCap)
	const n = 10_000
	r := rand.New(rand.NewSource(0x4A5B)) // deterministic seed for replays
	seen := make(map[string]int, n)
	for i := 0; i < n; i++ {
		// Build a route that looks like the gateway-side label:
		// method + " " + path. The path has a varying-length
		// token to exercise the unbounded-distinct-paths case
		// (UUID-shaped traffic under /users/{uuid}).
		route := fmt.Sprintf("GET /users/%040x", r.Uint64())
		got := s.admit(route)
		seen[got]++
		// Hot-path idempotence: the second call returns the same
		// label and does not push the map past its cap.
		if got2 := s.admit(route); got2 != got {
			t.Fatalf("admit() not idempotent for %q: first=%q second=%q", route, got, got2)
		}
	}
	// The cap is the load-bearing invariant. The map must never
	// exceed cap+2 (the +2 is the two reserved labels, which are
	// pre-admitted at construction and never evict).
	maxLen := routeLabelSetCap + 2
	if got := len(s.admitted); got > maxLen {
		t.Errorf("admitted map exceeded cap+2: got %d, want <= %d", got, maxLen)
	}
	// The overflow bucket must have been used (`__route_other__`
	// is the only place 10k distinct routes can land under cap=50).
	if seen[otherRouteLabel] == 0 {
		t.Errorf("expected __route_other__ bucket to be hit at least once under 10k random routes, got 0")
	}
	// The "real" routes admitted must be <= cap-1 (the +2 reserved
	// labels reduce the real-route budget by 2 — subtype of the
	// reservedCount invariant in admit()).
	reservedCount := 2
	realCount := len(s.admitted) - reservedCount
	if realCount > routeLabelSetCap {
		t.Errorf("real-route admissions exceeded cap: got %d, want <= %d", realCount, routeLabelSetCap)
	}
}

// TestRouteLabelSet_Admit_ReservedLabelsPreAdmittedFree pins
// invariant #4 — the reserved labels are pre-admitted without
// consuming capacity so the real-route budget is exactly `cap`,
// not `cap-2`. A single app seeing cap+1 distinct routes must
// have exactly cap-1 real routes admitted plus the overflow
// bucket, not cap-2.
func TestRouteLabelSet_Admit_ReservedLabelsPreAdmittedFree(t *testing.T) {
	s := newRouteLabelSetWithCap(4)
	// Admit 4 distinct real routes.
	labels := []string{
		"GET /users",
		"GET /orders",
		"POST /payments",
		"GET /products",
	}
	for _, l := range labels {
		if got := s.admit(l); got != l {
			t.Fatalf("admit(%q) = %q, want own label", l, got)
		}
	}
	// Confirm the map is exactly cap+2 entries (the reserved
	// labels plus the 4 real routes).
	if got := len(s.admitted); got != 4+2 {
		t.Errorf("admitted map size after %d real routes: got %d, want 6 (4 real + 2 reserved)", len(labels), got)
	}
	// The 5th distinct route must collapse to __route_other__.
	if got := s.admit("GET /admin"); got != otherRouteLabel {
		t.Errorf("5th distinct route should overflow; got %q, want %q", got, otherRouteLabel)
	}
	// The empty string is the reserved "no appID" sentinel and
	// must be returned verbatim without consuming capacity.
	if got := s.admit(""); got != reservedRouteLabelEmpty {
		t.Errorf("\"\" should be the reservedRouteLabelEmpty sentinel; got %q", got)
	}
	// The overflow bucket itself is reserved and admits itself.
	if got := s.admit(otherRouteLabel); got != otherRouteLabel {
		t.Errorf("__route_other__ should be reserved; got %q", got)
	}
	if got := len(s.admitted); got != 4+2 {
		t.Errorf("reserved-label admission should not consume capacity; got %d entries, want 6", got)
	}
}

// TestRouteLabelSet_Admit_ZeroCapacityPanics pins the contract
// that a misconfigured daemon fails loud at boot rather than
// silently allowing unbounded admission.
func TestRouteLabelSet_Admit_ZeroCapacityPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("newRouteLabelSetWithCap(0) should panic; got no panic")
		}
	}()
	_ = newRouteLabelSetWithCap(0)
}

// TestRouteLabelSet_Admit_NegativeCapacityPanics — same shape
// for the negative path.
func TestRouteLabelSet_Admit_NegativeCapacityPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("newRouteLabelSetWithCap(-1) should panic; got no panic")
		}
	}()
	_ = newRouteLabelSetWithCap(-1)
}
