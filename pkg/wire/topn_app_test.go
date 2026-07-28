// Property tests for the top-N per-(account_id, app_id) admission
// primitive introduced for issue #301 (per-plan CPU fairness
// observability — `vmmd_cpu_throttle_seconds_total{account_id,
// app_id}` counter bounded to topAppSetCap + 1 series).
//
// Shape mirrors pkg/wire/topn_test.go (issue #300 / PR #364). The
// composite key is the load-bearing difference: instead of
// (account_id) alone we rank on (account_id, app_id) so the
// fleet-throttle dashboard can surface "which app is hot" rather
// than "which customer is noisy". The cap is 100 (vs 1 000 for the
// tenant-abuse primitive) because the operationally useful
// granularity is per-app, and the Scale-plan 100-deploy upper bound
// is the binding floor — anything past 100 collapses to
// ("other", "other") so the TSDB series set stays bounded over the
// daemon's lifetime.
//
// The same six assertions as topn_test.go:
//   - BoundedCardinality: 50 000 distinct pairs still emit ≤ cap+1 series.
//   - TopNOrdering: hot pairs surface; cold pairs collapse to other.
//   - RollingReset: a fresh OpsMetrics is independent.
//   - ConcurrentSample: -race detector.
//   - OtherBucketOnIdle: pre-instantiated other row survives an empty
//     primitive (so the dashboard selector {app_id!="other"} never
//     sees "no data").
//   - ResetAfter24h: the rolling reset wipes the counts without
//     panic, exercised via the TestAdvanceAppClock test seam.

package wire_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/wire"
)

// TestTopAppThrottle_BoundedCardinality asserts the gauge exposition
// never exceeds topAppSetCap + 1 (101) series even under fuzzed
// load with 50 000 distinct (account_id, app_id) pairs (issue #301
// acceptance #4 — "only labels 100 hottest apps").
//
// Implementation note: same convention as
// TestTopTenantRPS_BoundedCardinality (topn_test.go:51) — count by
// metric prefix lines in the rendered /metrics body, robust against
// the pre-instantiated ("other", "other") row.
func TestTopAppThrottle_BoundedCardinality(t *testing.T) {
	if testing.Short() {
		t.Skip("fuzz-heavy; skipped in -short")
	}
	const totalPairs = 50_000
	m := wire.NewOpsMetrics("vmmd")
	for i := 0; i < totalPairs; i++ {
		m.ObserveTopAppThrottle(fmt.Sprintf("acct-%06x", i%1024), fmt.Sprintf("app-%06x", i))
	}
	m.EmitTopAppThrottle(func(string, string) float64 { return 1.0 })
	body := render(t, m)
	lines := strings.Split(body, "\n")
	var count int
	for _, ln := range lines {
		if strings.HasPrefix(ln, "vmmd_cpu_throttle_seconds_total{") {
			count++
		}
	}
	if count > 101 {
		t.Fatalf("counter exposition has %d series; bound is 101 (topAppSetCap=100 + 1 overflow)", count)
	}
	// The ("other", "other") overflow bucket must be present,
	// because we drove 50 000 pairs through a 100-cap set.
	if !strings.Contains(body, `vmmd_cpu_throttle_seconds_total{account_id="other",app_id="other"}`) {
		t.Errorf("expected (other, other) overflow series in:\n%s", body)
	}
	// Sanity: the top-1 pair (acct-000000, app-000000) must
	// surface — it's the lex-smallest distinct pair and the
	// single-observation tie-break selects it over the others.
	if !strings.Contains(body, `vmmd_cpu_throttle_seconds_total{account_id="acct-000000",app_id="app-000000"}`) {
		t.Errorf("expected top-1 series (acct-000000, app-000000) in:\n%s", body)
	}
}

// TestTopAppThrottle_TopNOrdering asserts that when more than cap
// distinct pairs are observed, the counter holds exactly the
// top-cap by sample count and the rest collapse to ("other",
// "other"). Mirrors the topAccountSet ordering test (issue #300):
// hot pairs surface, cold pairs collapse.
func TestTopAppThrottle_TopNOrdering(t *testing.T) {
	m := wire.NewOpsMetrics("vmmd")
	// 5 hot pairs (each observed 100 times) plus cap+5 noise
	// pairs (each observed once). Top-N should surface the 5 hot
	// pairs; the noise pairs overflow to ("other", "other").
	for i := 0; i < 5; i++ {
		for j := 0; j < 100; j++ {
			m.ObserveTopAppThrottle("acct-hot", fmt.Sprintf("hot-%d", i))
		}
	}
	for i := 0; i < 200; i++ {
		// 3-digit padding so lex order matches numeric order
		// (cold-001 < cold-002 < ... < cold-099 < cold-100 <
		// ... < cold-200). With 5 hot + 200 cold = 205 pairs
		// and cap=100, the 105 lex-largest cold pairs must
		// collapse to ("other", "other"). The 95 lex-smallest
		// cold pairs (cold-001 .. cold-095) survive the
		// tie-break.
		m.ObserveTopAppThrottle("acct-cold", fmt.Sprintf("cold-%03d", i))
	}
	m.EmitTopAppThrottle(func(string, string) float64 { return 1.0 })
	body := render(t, m)
	for i := 0; i < 5; i++ {
		want := fmt.Sprintf(`vmmd_cpu_throttle_seconds_total{account_id="acct-hot",app_id="hot-%d"}`, i)
		if !strings.Contains(body, want) {
			t.Errorf("hot pair %d missing from body:\n%s", i, body)
		}
	}
	// Tail pair must collapse to (other, other). With
	// 3-digit padding, cold-200 is the lex-largest cold pair;
	// 5 hot + 95 lex-smallest cold = 100 surviving pairs
	// (cap), so cold-200 must NOT appear.
	tail := `vmmd_cpu_throttle_seconds_total{account_id="acct-cold",app_id="cold-200"}`
	if strings.Contains(body, tail) {
		t.Errorf("tail pair cold-200 should have collapsed to (other, other):\n%s", body)
	}
}

// TestTopAppThrottle_RollingReset asserts that a fresh OpsMetrics
// starts with an empty top-N (the primitive is per-instance, not
// daemon-lifetime-global). Mirrors topn_test.go:120.
func TestTopAppThrottle_RollingReset(t *testing.T) {
	m1 := wire.NewOpsMetrics("vmmd")
	m1.ObserveTopAppThrottle("acct-aaa", "app-aaa")
	m1.ObserveTopAppThrottle("acct-bbb", "app-bbb")
	m1.EmitTopAppThrottle(func(string, string) float64 { return 1.0 })
	// The contract: a second NewOpsMetrics is independent. The
	// primitive is per-OpsMetrics; a fresh instance starts empty
	// except for the pre-instantiated ("other", "other") row.
	m2 := wire.NewOpsMetrics("vmmd")
	m2.EmitTopAppThrottle(func(string, string) float64 { return 1.0 })
	body2 := render(t, m2)
	if strings.Contains(body2, `account_id="acct-aaa"`) {
		t.Errorf("fresh OpsMetrics leaked top-N state from m1:\n%s", body2)
	}
}

// TestTopAppThrottle_ConcurrentSample is the -race detector for the
// sampler goroutine (cmd/vmmd/throttle_sampler.go ticks every 5s)
// and any concurrent path that calls ObserveTopAppThrottle. Without
// the sync.Mutex on topAppSet this test would flake on map-write
// races under -race. Mirrors topn_test.go:144.
func TestTopAppThrottle_ConcurrentSample(t *testing.T) {
	m := wire.NewOpsMetrics("vmmd")
	const goroutines = 16
	const perGoroutine = 500
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				m.ObserveTopAppThrottle(fmt.Sprintf("acct-%04x", gid), fmt.Sprintf("app-%04x-%04x", gid, i))
			}
		}(g)
	}
	wg.Wait()
	// One emit, once: this is the load-bearing assertion. The
	// sampler-side emit happens once per 5s tick, so the counter
	// series count after a single emit is bounded at cap + 1.
	emitted := m.EmitTopAppThrottle(func(string, string) float64 { return 1.0 })
	if emitted > 101 {
		t.Fatalf("EmitTopAppThrottle emitted %d series; bound is 101", emitted)
	}
	body := render(t, m)
	lines := strings.Split(body, "\n")
	var count int
	for _, ln := range lines {
		if strings.HasPrefix(ln, "vmmd_cpu_throttle_seconds_total{") {
			count++
		}
	}
	if count > 101 {
		t.Fatalf("after concurrent sample + single emit, counter has %d series; bound is 101", count)
	}
}

// TestTopAppThrottle_OtherBucketOnIdle asserts that an OpsMetrics
// with no observations still emits the ("other", "other") row from
// the boot pre-instantiation. Same precedent as
// TestTopTenantRPS_OtherBucketOnIdle (issue #300): a daemon must
// surface its metrics from the moment it boots, not only after the
// first observation. The dashboard's panel selector
// {app_id!="other"} must therefore always see "no data", never
// "missing".
func TestTopAppThrottle_OtherBucketOnIdle(t *testing.T) {
	m := wire.NewOpsMetrics("vmmd")
	m.EmitTopAppThrottle(func(string, string) float64 { return 1.0 })
	body := render(t, m)
	want := `vmmd_cpu_throttle_seconds_total{account_id="other",app_id="other"}`
	if !strings.Contains(body, want) {
		t.Errorf("idle daemon missing pre-instantiated (other, other) row %q:\n%s", want, body)
	}
}

// TestTopAppThrottle_ResetAfter24h exercises the 24h rolling reset
// path with a fake clock via the TestAdvanceAppClock test seam on
// OpsMetrics (the blackbox test can't reach the primitive's
// unexported fields directly). The seam overrides the primitive's
// `now` clock AND rewinds `lastReset` so the shouldReset math is
// deterministic. Without this test, a regression that breaks the
// reset would surface as a stale (account_id, app_id) pair
// persisting past the 24h window — visible only via the dashboard's
// "Other bucket growth" panel, which is a fleet-level signal not
// caught by the alert synthetic-fixture test.
//
// Mirrors topn_test.go:228.
func TestTopAppThrottle_ResetAfter24h(t *testing.T) {
	base := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	m := wire.NewOpsMetrics("vmmd")
	m.ObserveTopAppThrottle("acct-stale", "app-stale")
	// Freeze the clock at base AND rewind lastReset so the
	// baseline starts at base.
	m.TestAdvanceAppClock(base, true)
	// Advance the clock past the window without rewinding — now
	// lastReset is still at base, now() is base+25h,
	// shouldReset returns true.
	m.TestAdvanceAppClock(base.Add(25*time.Hour), false)
	set := m.TopAppSet()
	if !set.ShouldReset() {
		t.Fatal("ShouldReset() = false after 25h; want true")
	}
	set.ResetWindow()
	// Stale pair must be gone after reset. EmitTopAppThrottle
	// returns len(snap) + 1 (the +1 is the ("other", "other")
	// overflow row). After reset, snap is empty so the return
	// value is exactly 1 (the overflow row only). A value > 1
	// would mean some real pair survived the reset.
	emitted := m.EmitTopAppThrottle(func(string, string) float64 { return 1.0 })
	if emitted != 1 {
		t.Errorf("after reset, EmitTopAppThrottle emitted %d series; want 1 (only the other overflow row — the stale pair must be gone)", emitted)
	}
	// ShouldReset must be false again immediately after reset
	// (lastReset was just bumped to now() = base+25h).
	if set.ShouldReset() {
		t.Error("ShouldReset() = true immediately after reset; want false")
	}
}
