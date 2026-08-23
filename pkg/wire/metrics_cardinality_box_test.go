// Tests for the bounded box-label admission set introduced for
// issue #1059 / ADR-127 (operator-facing wake failure-mode
// observability). The set lives on OpsMetrics and backs the
// wakeFailure and wakeLatency metrics (box label) so the Prometheus
// TSDB series set stays bounded under a hostile config or a future
// compute_nodes roll-out that mints more boxes than the cap.
//
// The shape of these tests is load-bearing: they pin the contract
// that
//   - the first maxBoxLabelValues (= 64) distinct box identifiers
//     are admitted,
//   - further box identifiers collapse to "__other__" without
//     consuming capacity,
//   - the reserved "local" and "__other__" labels never count against
//     the cap,
//   - the admission set is race-safe under -race,
//   - repeated lookups for the same box identifier return the same
//     underlying Prometheus Counter (counter monotonicity).
//
// The pattern mirrors metrics_cardinality_ip_test.go which exercises
// the sibling ipLabelSet for issue #286, and metrics_cardinality_test.go
// which exercises accountLabelSet for issue #278. The three tests
// are written as siblings so a future cardinality change has parity
// evidence across all three label sets.

package wire_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/onebox-faas/faas/pkg/wire"
)

// TestWakeFailure_FirstNBoxesAdmitted asserts the first
// maxBoxLabelValues distinct box identifiers round-trip unchanged
// through the box-labelled counter. We sub-sample at 64 (the full
// cap) — the cap is exercised by the overflow test below.
func TestWakeFailure_FirstNBoxesAdmitted(t *testing.T) {
	const n = 64
	m := wire.NewOpsMetrics("vmmd")
	for i := 0; i < n; i++ {
		box := fmt.Sprintf("box-%02d", i)
		// Force admission by hitting the wake-failure counter once.
		m.WakeFailure(box, "snapshot_restore_err").Inc()
	}
	body := render(t, m)
	for i := 0; i < n; i++ {
		box := fmt.Sprintf("box-%02d", i)
		want := fmt.Sprintf(`vmmd_wake_failure_total{box=%q,reason="snapshot_restore_err"} 1`, box)
		if !strings.Contains(body, want) {
			t.Errorf("missing admitted series %q in:\n%s", want, body)
		}
	}
}

// TestWakeFailure_OverflowCollapsesToOther is the canonical overflow
// driver (issue #1059 / ADR-127): drive maxBoxLabelValues + 1 = 65
// distinct box identifiers through WakeFailure and assert the 65th
// collapses to "__other__" (the bounded admission overflow bucket).
// The test is intentionally cheap — 65 boxes is small enough to
// stay in the fast `make test` loop. The "what about 1000 boxes /
// maxAccountLabelValues - reservedCount" check lives in
// metrics_cardinality_test.go::TestAccountLabel_OverflowCollapsesToOther
// for the account_id admission set; the box admission set is
// analogous so we don't duplicate the deep driver here.
func TestWakeFailure_OverflowCollapsesToOther(t *testing.T) {
	m := wire.NewOpsMetrics("vmmd")
	// maxBoxLabelValues = 64 distinct real boxes get admitted.
	// The 65th distinct box must collapse to the reserved
	// "__other__" bucket.
	for i := 0; i < 65; i++ {
		box := fmt.Sprintf("box-%02d", i)
		m.WakeFailure(box, "snapshot_restore_err").Inc()
	}
	body := render(t, m)

	// Spot-check the overflow bucket: the 65th box (box-64) must
	// NOT have a per-box series in the scrape output — its
	// increment must have collapsed to "__other__".
	if strings.Contains(body, `vmmd_wake_failure_total{box="box-64",reason="snapshot_restore_err"}`) {
		t.Errorf("overflow box-64 unexpectedly admitted (should collapse to __other__):\n%s", body)
	}

	// The 64 admitted boxes (box-00 through box-63) must each
	// surface with count 1.
	for i := 0; i < 64; i++ {
		box := fmt.Sprintf("box-%02d", i)
		want := fmt.Sprintf(`vmmd_wake_failure_total{box=%q,reason="snapshot_restore_err"} 1`, box)
		if !strings.Contains(body, want) {
			t.Errorf("missing admitted series %q in:\n%s", want, body)
		}
	}

	// The reserved "__other__" bucket must surface with count 1
	// (the 65th increment collapsed here).
	want := `vmmd_wake_failure_total{box="__other__",reason="snapshot_restore_err"} 1`
	if !strings.Contains(body, want) {
		t.Errorf("missing overflow bucket series %q in:\n%s", want, body)
	}
}

// TestWakeFailure_ReservedLabelsNeverAgainstCap asserts the
// reserved values (labelLocal, otherBoxLabel) are admitted at boot
// without consuming capacity. The check is observational: the
// reserved labels show up in the pre-instantiated series with value
// 0 even when no real box has been admitted yet, which is the
// contract the metric-reader relies on for "no data" detection.
func TestWakeFailure_ReservedLabelsNeverAgainstCap(t *testing.T) {
	m := wire.NewOpsMetrics("vmmd")
	body := render(t, m)
	for _, want := range []string{
		`vmmd_wake_failure_total{box="local",reason="snapshot_stale"} 0`,
		`vmmd_wake_failure_total{box="__other__",reason="snapshot_stale"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing reserved-label series %q in:\n%s", want, body)
		}
	}
}

// TestWakeFailure_SameBoxReturnsSameCounter proves the admission
// set is deduplicating: two calls for the same box identifier
// return the same underlying Prometheus Counter, so the counter is
// monotonic. The flag is tripped by a regression that admitted each
// lookup as a new label (a frozenset collapse would lose the
// monotonicity).
func TestWakeFailure_SameBoxReturnsSameCounter(t *testing.T) {
	m := wire.NewOpsMetrics("vmmd")
	a := m.WakeFailure("box-dup", "netns_fail")
	b := m.WakeFailure("box-dup", "netns_fail")
	if a == nil || b == nil {
		t.Fatal("counter is nil")
	}
	aDesc := a.Desc().String()
	bDesc := b.Desc().String()
	if aDesc != bDesc {
		t.Errorf("counter Desc mismatch: %q vs %q", aDesc, bDesc)
	}
	a.Inc()
	a.Inc()
	a.Inc()
	b.Inc()
	b.Inc()
	// Five total increments land on the same counter, so the
	// registry surface shows 5.
	body := render(t, m)
	if !strings.Contains(body, `vmmd_wake_failure_total{box="box-dup",reason="netns_fail"} 5`) {
		t.Errorf("expected box-dup series at 5 after 5 increments, got:\n%s", body)
	}
}

// TestWakeFailure_RaceSafe asserts the admission set is
// goroutine-safe under -race. The workload is N goroutines each
// admitting K distinct box identifiers — the contention is the
// lookup/insert path on the underlying map. A regression that
// dropped the mutex would trip the -race detector with a
// concurrent map read/write.
func TestWakeFailure_RaceSafe(t *testing.T) {
	m := wire.NewOpsMetrics("vmmd")
	const goroutines = 8
	const perG = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				box := fmt.Sprintf("race-%d-%d", g, i)
				m.WakeFailure(box, "jailer_fail").Inc()
			}
		}(g)
	}
	wg.Wait()
	// No counter is asserted by value here — the test passes iff
	// the race detector fires on the contended map access. The
	// surface counter check is a sanity tieout.
	body := render(t, m)
	if !strings.Contains(body, "vmmd_wake_failure_total") {
		t.Errorf("missing wake-failure counter in scrape output:\n%s", body)
	}
}
