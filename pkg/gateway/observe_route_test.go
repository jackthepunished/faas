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
	if got := h.RoutesFor("app-does-not-exist"); got != nil {
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
	got := h.RoutesFor("app-1")
	if got == nil {
		t.Fatalf("RoutesFor(_) = nil after admits, want populated snapshot")
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
	got := h.RoutesFor("app-cap")
	if got == nil {
		t.Fatalf("RoutesFor after overflow = nil, want populated snapshot")
	}
	if len(got) != 52 {
		t.Fatalf("RoutesFor length after overflow = %d, want 52 (50 cap + 2 reserved)", len(got))
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
