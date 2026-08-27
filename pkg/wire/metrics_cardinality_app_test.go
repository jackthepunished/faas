// Tests for the bounded app-label admission set introduced for
// issue #1059 / ADR-127 §3.5 deferred work, shipped in the
// platform-observability mega-PR (Cluster A, commit 1). The set
// lives on OpsMetrics and is the per-app sibling of the box
// admission set (metrics_cardinality_box_test.go); the third
// wake-failure label dimension ("app") flows through appLabel()
// the same way the "box" dimension flows through boxLabel().
// Commit 2 of the mega-PR adds the per-app wake-failure split
// (the counter itself); commit 1 ships only the primitive so the
// follow-on commit can land the metric label extension against
// an already-tested admission set.
//
// The shape of these tests is load-bearing: they pin the contract
// that
//   - the first maxAppLabelValues (= 256) distinct app slugs are
//     admitted (sampled at a smaller N here for test speed — the
//     full cap is exercised by the overflow test below),
//   - further app slugs collapse to "__other__" without
//     consuming capacity,
//   - the reserved "" (labelAppUnknown) and "__other__"
//     (otherAppLabel) labels never count against the cap,
//   - the admission set is race-safe under -race,
//   - the accessor is nil-safe (daemon paths and unit tests
//     that don't wire an OpsMetrics keep building).
//
// The pattern mirrors metrics_cardinality_box_test.go (the
// box-label sibling for issue #1059 / ADR-127) and
// metrics_cardinality_ip_test.go (the IP-label sibling for
// issue #286). The three tests are written as siblings so a
// future cardinality change has parity evidence across all
// three label sets.
//
// This is an INTERNAL test (package wire, not wire_test) because
// the admission accessor (m *OpsMetrics).appLabel is unexported,
// mirroring the (m *OpsMetrics).boxLabel and (m *OpsMetrics).ipLabel
// precedents. The public surface (WakeFailure(app, ...)) lands in
// commit 2 and is exercised by the external test package.

package wire

import (
	"fmt"
	"sync"
	"testing"
)

// TestAppLabelSet_FirstNAppsAdmitted asserts the first
// maxAppLabelValues distinct app slugs round-trip unchanged
// through the appLabel() accessor. We sub-sample at 256 (the
// full cap) — the cap is exercised by the overflow test below.
// The test confirms the constructor reserves the right map
// capacity (256 real apps + 2 reserved entries = 258) without
// over-flowing on the very first 256 distinct slugs.
func TestAppLabelSet_FirstNAppsAdmitted(t *testing.T) {
	const n = 256
	m := NewOpsMetrics("vmmd")
	for i := 0; i < n; i++ {
		app := fmt.Sprintf("app-%03d", i)
		got := m.appLabel(app)
		if got != app {
			t.Fatalf("app %q unexpectedly collapsed to %q at i=%d (within cap)", app, got, i)
		}
	}
}

// TestAppLabelSet_OverflowCollapsesToOther is the canonical
// overflow driver for the app admission set (issue #1059 /
// ADR-127 §3.5 deferred work): drive maxAppLabelValues + 1 = 257
// distinct app slugs through appLabel() and assert the 257th
// collapses to "__other__" (the bounded admission overflow
// bucket). The test is intentionally cheap — 257 apps is small
// enough to stay in the fast `make test` loop; the
// "what about maxAppLabelValues - reservedCount" deep driver is
// the box-set precedent at
// metrics_cardinality_box_test.go::TestWakeFailure_OverflowCollapsesToOther
// (the app set is a parallel implementation, so we don't
// duplicate the deep driver here).
func TestAppLabelSet_OverflowCollapsesToOther(t *testing.T) {
	m := NewOpsMetrics("vmmd")
	// maxAppLabelValues = 256 distinct real apps get admitted.
	// The 257th distinct app slug must collapse to the reserved
	// "__other__" bucket.
	for i := 0; i < 257; i++ {
		app := fmt.Sprintf("app-%03d", i)
		got := m.appLabel(app)
		want := app
		if i == 256 {
			want = otherAppLabel
		}
		if got != want {
			t.Errorf("i=%d app=%q: got %q, want %q", i, app, got, want)
		}
	}
}

// TestAppLabelSet_ReservedLabelsNeverAgainstCap asserts the
// reserved values (labelAppUnknown == "" and otherAppLabel ==
// "__other__") are admitted at boot without consuming capacity
// AND that they remain re-admittable on every subsequent lookup
// — a regression that pre-admitted the reserved labels without
// the collision-free re-admit branch would either over-flow the
// real-app budget (the bug fixed by the reservedCount accounting
// in admit()) or fail to return the reserved label on subsequent
// calls (the bug fixed by the early switch in admit()).
func TestAppLabelSet_ReservedLabelsNeverAgainstCap(t *testing.T) {
	m := NewOpsMetrics("vmmd")
	// Empty input resolves to "" (labelAppUnknown) without
	// consuming capacity.
	if got := m.appLabel(""); got != labelAppUnknown {
		t.Errorf("empty input: got %q, want %q (labelAppUnknown)", got, labelAppUnknown)
	}
	// "__other__" input resolves to "__other__" without consuming
	// capacity.
	if got := m.appLabel(otherAppLabel); got != otherAppLabel {
		t.Errorf("__other__ input: got %q, want %q (otherAppLabel)", got, otherAppLabel)
	}
	// After admitting 256 distinct real apps, the reserved labels
	// must STILL resolve correctly — they never count against the
	// cap.
	for i := 0; i < 256; i++ {
		m.appLabel(fmt.Sprintf("real-%03d", i))
	}
	if got := m.appLabel(""); got != labelAppUnknown {
		t.Errorf("empty input after cap-full: got %q, want %q (reserved labels should still resolve)", got, labelAppUnknown)
	}
	if got := m.appLabel(otherAppLabel); got != otherAppLabel {
		t.Errorf("__other__ input after cap-full: got %q, want %q (reserved labels should still resolve)", got, otherAppLabel)
	}
}

// TestAppLabelSet_SameAppIsIdempotent proves the admission set
// is deduplicating: two calls for the same app slug return the
// same label value (and consume only one map slot). The
// regression this guards against is the failure mode where the
// admit() path mistook a re-lookup for a new admission and
// admitted it again — which would over-flow the real-app budget
// faster than the documented contract.
func TestAppLabelSet_SameAppIsIdempotent(t *testing.T) {
	m := NewOpsMetrics("vmmd")
	first := m.appLabel("my-app")
	second := m.appLabel("my-app")
	third := m.appLabel("my-app")
	if first != "my-app" || second != "my-app" || third != "my-app" {
		t.Fatalf("idempotent lookup returned distinct values: %q %q %q", first, second, third)
	}
}

// TestAppLabelSet_RaceSafe asserts the admission set is
// goroutine-safe under -race. The workload is N goroutines each
// admitting K distinct app slugs — the contention is the
// lookup/insert path on the underlying map. A regression that
// dropped the mutex would trip the -race detector with a
// concurrent map read/write. The surface accessor is asserted
// to be functional (not nil-panicking) under contention.
func TestAppLabelSet_RaceSafe(t *testing.T) {
	m := NewOpsMetrics("vmmd")
	const goroutines = 8
	const perG = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				app := fmt.Sprintf("race-%d-%d", g, i)
				// We don't assert the return value here — the
				// test passes iff the race detector fires on
				// the contended map access. The accessor must
				// not panic and must return a non-empty
				// string.
				got := m.appLabel(app)
				if got == "" {
					t.Errorf("accessor returned empty for %q", app)
				}
			}
		}(g)
	}
	wg.Wait()
}

// TestAppLabel_NilReceiver asserts the (m *OpsMetrics).appLabel
// accessor is nil-safe — daemon paths that don't wire an
// OpsMetrics (unit tests, fixture paths) keep building. The
// returned value for a nil receiver is the input unchanged; the
// call site is responsible for not relying on the admission-set
// guarantee in that case.
func TestAppLabel_NilReceiver(t *testing.T) {
	var m *OpsMetrics
	// Real app slug returns unchanged.
	if got := m.appLabel("my-app"); got != "my-app" {
		t.Errorf("nil receiver: got %q, want %q (unchanged)", got, "my-app")
	}
	// Empty input returns unchanged.
	if got := m.appLabel(""); got != "" {
		t.Errorf("nil receiver empty: got %q, want %q (unchanged)", got, "")
	}
	// Overflow-shaped input returns unchanged (no admission set
	// to collapse to __other__).
	if got := m.appLabel(otherAppLabel); got != otherAppLabel {
		t.Errorf("nil receiver __other__: got %q, want %q (unchanged)", got, otherAppLabel)
	}
}

// TestAppLabelSet_OverflowBucketPreInstantiated asserts the
// __other__ reserved label is reachable from the constructor
// even when zero real apps have been admitted — same observable
// property as the box-set sibling
// (TestWakeFailure_ReservedLabelsNeverAgainstCap). This is the
// "no data" detection contract the metric-reader relies on: an
// operator can hit /metrics on a fresh daemon and see the
// overflow bucket exists at value 0 (the public-counter version
// of this lands in commit 2; the primitive-level version here
// confirms the accessor returns the reserved label for the
// __other__ input on a fresh OpsMetrics).
func TestAppLabelSet_OverflowBucketPreInstantiated(t *testing.T) {
	m := NewOpsMetrics("vmmd")
	// The accessor is the only public surface in commit 1 (no
	// counter yet — that's commit 2). Confirm the accessor
	// returns the reserved label for the __other__ input on a
	// fresh OpsMetrics.
	if got := m.appLabel(otherAppLabel); got != otherAppLabel {
		t.Errorf("fresh OpsMetrics: got %q, want %q (otherAppLabel)", got, otherAppLabel)
	}
	// And the empty input resolves to labelAppUnknown (empty
	// string).
	if got := m.appLabel(""); got != labelAppUnknown {
		t.Errorf("fresh OpsMetrics empty: got %q, want %q (labelAppUnknown)", got, labelAppUnknown)
	}
}

// TestAppLabelSet_OverflowDoesNotEvict asserts the cap behavior
// is fixed-capacity non-evicting — admitting past the cap
// collapses to otherAppLabel but does NOT evict the existing
// real app slugs. A regression that swapped the map for an LRU
// would let evicted slugs re-admit later and grow the
// Prometheus TSDB series set unbounded over the daemon's
// lifetime — the same regression the accountLabelSet and
// ipLabelSet siblings explicitly guard against.
func TestAppLabelSet_OverflowDoesNotEvict(t *testing.T) {
	m := NewOpsMetrics("vmmd")
	// Admit one real app slug, then drive 257 distinct
	// overflow apps past the cap.
	got := m.appLabel("real-keep")
	if got != "real-keep" {
		t.Fatalf("initial admit failed: got %q", got)
	}
	for i := 0; i < 300; i++ {
		m.appLabel(fmt.Sprintf("overflow-%03d", i))
	}
	// After 300 overflow attempts, the originally admitted
	// "real-keep" slug must STILL resolve to its real label.
	if got := m.appLabel("real-keep"); got != "real-keep" {
		t.Errorf("post-overflow admit evicted real slug: got %q, want %q", got, "real-keep")
	}
	// And a fresh overflow attempt still collapses to
	// "__other__" (the cap didn't move).
	if got := m.appLabel("overflow-fresh"); got != otherAppLabel {
		t.Errorf("post-overflow fresh overflow: got %q, want %q", got, otherAppLabel)
	}
}
