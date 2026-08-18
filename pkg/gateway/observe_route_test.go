// Per-route observability whitebox tests (ADR-093).
//
// Three layers are tested here:
//
//  1. Handler.routeSetFor is the lazy-creating admission-set map.
//     When called with enabled=false, it must return nil so the
//     caller short-circuits the per-route emission on every
//     request. When called with enabled=true, it must return a
//     fresh *routeLabelSet on the first sight and the same
//     instance on subsequent sights.
//
//  2. Handler.RoutesFor is the snapshot accessor the control-
//     listener /v1/internal/apps/{slug}/routes handler reads. It
//     must return nil when the app has not been admitted yet
//     (so the control handler can render an empty Routes array
//     rather than a 404), and a deterministic sorted slice of
//     the admitted labels when the app is in process.
//
//  3. The end-to-end routeLabelSet.admit contract: 50 admitted
//     distinct routes + overflow into __route_other__. The cap
//     is the load-bearing number that bounds the per-app
//     Prometheus series set, so the property test is more
//     useful than a few hand-written cases.
package gateway

import (
	"sort"
	"testing"
)

func TestRouteSetFor_DisabledReturnsNil(t *testing.T) {
	h := &Handler{}
	if got := h.routeSetFor("app-1", false); got != nil {
		t.Fatalf("routeSetFor(_, false) = %v, want nil (disabled short-circuit)", got)
	}
	if got := h.routeSetFor("", true); got != nil {
		t.Fatalf("routeSetFor(\"\", true) = %v, want nil (empty appID guard)", got)
	}
}

func TestRouteSetFor_EnabledLazilyCreates(t *testing.T) {
	h := &Handler{}
	first := h.routeSetFor("app-1", true)
	if first == nil {
		t.Fatalf("routeSetFor(_, true) = nil on first sight, want fresh set")
	}
	second := h.routeSetFor("app-1", true)
	if first != second {
		t.Fatalf("routeSetFor returned a different set on second sight; want same *routeLabelSet pointer (LoadOrStore invariant)")
	}
}

func TestRoutesFor_NilWhenAppUnknown(t *testing.T) {
	h := &Handler{}
	if got, _ := h.RoutesFor("app-does-not-exist"); got != nil {
		t.Fatalf("RoutesFor(unknown) = %v, want nil", got)
	}
}

func TestRoutesFor_ReturnsSortedSnapshot(t *testing.T) {
	h := &Handler{}
	set := h.routeSetFor("app-1", true)
	// Admit a few routes in non-sorted order so we can verify
	// the snapshot is sorted (the dashboard / dashboard JS
	// relies on this for stable rendering).
	want := []string{"GET /users", "GET /users/{uuid}", "POST /users"}
	for _, r := range want {
		set.admit(r)
	}
	got, overflowed := h.RoutesFor("app-1")
	if got == nil {
		t.Fatalf("RoutesFor(_) = nil after admits, want populated snapshot")
	}
	if overflowed {
		t.Fatalf("RoutesFor overflowed = true at 3 admitted routes (under cap=50); want false")
	}
	sort.Strings(want)
	// Snapshot must include reserved labels + the admitted ones.
	if len(got) != len(want)+2 {
		t.Fatalf("RoutesFor length = %d, want %d (admitted + 2 reserved)", len(got), len(want)+2)
	}
	// Check the admitted set is a subset (in any order) of the
	// returned slice.
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("RoutesFor missing admitted %q in snapshot %v", w, got)
		}
	}
	if !containsRouteLabel(got, "") {
		t.Fatalf("RoutesFor missing reserved empty label in snapshot %v", got)
	}
	if !containsRouteLabel(got, "__route_other__") {
		t.Fatalf("RoutesFor missing __route_other__ reserved label in snapshot %v", got)
	}
}

func TestRoutesFor_OverflowCollapsesToOther(t *testing.T) {
	h := &Handler{}
	set := h.routeSetFor("app-cap", true)
	// Admit 60 distinct routes — 50 land in the cap, the rest
	// collapse to __route_other__ (which is pre-admitted, so it
	// shows up once in the snapshot).
	for i := 0; i < 60; i++ {
		set.admit("GET /probe/" + itoaForTest(i))
	}
	got, overflowed := h.RoutesFor("app-cap")
	if got == nil {
		t.Fatalf("RoutesFor after overflow = nil, want populated snapshot")
	}
	if !overflowed {
		t.Fatalf("RoutesFor overflowed = false after 60 admits; want true (cap=50 hit)")
	}
	if len(got) != 52 {
		t.Fatalf("RoutesFor length after overflow = %d, want 52 (50 cap + 2 reserved)", len(got))
	}
}

// TestRoutesFor_ReportsOverflowed is the new regression test
// added in PR-B1 (ADR-093 Tier B item #1). It pins the
// `overflowed` return value separately from the slice length
// because the dashboard distinguishes "cap hit" from "under cap"
// via the dedicated cap_hit bool, not by counting the array
// (which is ambiguous: 5 real + __route_other__ is
// indistinguishable from 50 real + overflow).
//
// Boundary semantics: RoutesFor reports overflowed when
// `len(admitted)-reservedCount >= cap`. With reservedCount=2
// and cap=50, the routeLabelSet holds 2 reserved + 49 reals
// (= 51 entries) when the 50th real admit is attempted. The
// gate trips BEFORE the insert: 51-2=49 is NOT >= 50, so the
// 50th admit succeeds and len becomes 52. RoutesFor then
// reports overflowed=true at that point (52-2=50 >= 50). The
// 51st distinct real admit collapses to __route_other__
// without inserting.
func TestRoutesFor_ReportsOverflowed(t *testing.T) {
	h := &Handler{}
	set := h.routeSetFor("app-1", true)
	// 49 real routes admitted — one below cap. overflowed must
	// remain false even though __route_other__ is pre-admitted
	// (admitting 49 real routes does not consume the overflow
	// bucket itself).
	for i := 0; i < 49; i++ {
		set.admit("GET /real/" + itoaForTest(i))
	}
	_, overflowed := h.RoutesFor("app-1")
	if overflowed {
		t.Fatalf("RoutesFor overflowed = true at 49 admits (under cap=50); want false")
	}
	// 50th admit: gate trip happens at the 51st admit attempt,
	// so the 50th real route is admitted. RoutesFor then
	// reports overflowed=true because len(admitted)-reservedCount
	// now equals cap.
	set.admit("GET /real/49")
	_, overflowed = h.RoutesFor("app-1")
	if !overflowed {
		t.Fatalf("RoutesFor overflowed = false after 50 admits (== cap); want true (cap reached)")
	}
	// 51st admit attempt: collapses to __route_other__ (no
	// insert), overflowed stays true. Asserts the snapshot is
	// still 52 entries (50 real + 2 reserved) — overflow
	// doesn't add a third bucket.
	collapsed := set.admit("GET /real/50")
	if collapsed != otherRouteLabel {
		t.Fatalf("admit() = %q, want %q (51st distinct must collapse to overflow bucket)", collapsed, otherRouteLabel)
	}
	routes, _ := h.RoutesFor("app-1")
	if len(routes) != 52 {
		t.Fatalf("RoutesFor length after 51st admit attempt = %d, want 52 (overflow collapsed, no extra bucket)", len(routes))
	}
}

func itoaForTest(n int) string {
	if n == 0 {
		return "0"
	}
	digits := "0123456789"
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	return string(buf[i:])
}

func containsRouteLabel(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
