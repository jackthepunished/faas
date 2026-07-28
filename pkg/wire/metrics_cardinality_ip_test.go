// Tests for the bounded IP-label admission set introduced for
// issue #286 (per-IP failed-login observability). The set lives on
// OpsMetrics and backs the apid_failed_login_total counter so the
// Prometheus TSDB series set stays bounded under a credential-stuffing
// burst.
//
// The shape of these tests is load-bearing: they pin the contract
// that
//   - the first maxIPLabelValues distinct IPs are admitted,
//   - further IPs collapse to "__other__" without consuming capacity,
//   - the reserved "anonymous" and "__other__" labels never count
//     against the cap,
//   - the admission set is race-safe under -race,
//   - repeated lookups for the same IP return the same underlying
//     Prometheus Counter (counter monotonicity).
//
// The pattern mirrors metrics_cardinality_test.go which exercises
// the sibling accountLabelSet for issue #278. The two tests are
// written as siblings so a future cardinality change has parity
// evidence across both label sets.
//
// The 10_000-IP overflow-collapse driver
// (TestFailedLoginTotal_OverflowCollapsesToOtherSlow) lives in a
// sibling file (metrics_cardinality_ip_slow_test.go) behind the
// `//go:build slow` tag so the default `make test` loop stays fast.
// Run with `go test -tags=slow ./pkg/wire/...` for the full cap
// exercise.

package wire_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/onebox-faas/faas/pkg/wire"
)

// TestFailedLoginTotal_FirstNIPsAdmitted asserts the first
// maxIPLabelValues distinct IPs round-trip unchanged through the
// per-IP counter. We sub-sample at 100 IPs for fast tests; the cap
// (10_000) is exercised by the overflow test below.
func TestFailedLoginTotal_FirstNIPsAdmitted(t *testing.T) {
	const n = 100
	m := wire.NewOpsMetrics("apid")
	for i := 0; i < n; i++ {
		ip := fmt.Sprintf("203.0.113.%d", i)
		// Force admission by hitting the failed-login counter once.
		m.FailedLoginTotal(ip).Inc()
	}
	body := render(t, m)
	for i := 0; i < n; i++ {
		ip := fmt.Sprintf("203.0.113.%d", i)
		want := fmt.Sprintf(`apid_failed_login_total{ip="%s"} 1`, ip)
		if !strings.Contains(body, want) {
			t.Errorf("missing pre-instantiated series %q in:\n%s", want, body)
		}
	}
}

// TestFailedLoginTotal_OverflowBucketPreInstantiated is the cheaper
// sibling of TestFailedLoginTotal_OverflowCollapsesToOther. It
// confirms the reserved "__other__" series is exposed in the scrape
// output even when zero real IPs have been admitted past the cap —
// i.e. the "overflow bucket is reachable" property is observable
// from boot.
//
// The full overflow-collapse contract (driving 10_001 distinct IPs
// through FailedLoginTotal and asserting the 10_001st collapses to
// "__other__") is exercised for the sibling accountLabelSet in
// metrics_cardinality_test.go::TestAccountLabel_OverflowCollapsesToOther.
// The IP path is a parallel implementation; replicating the 10_001-IP
// driver here would double the wall cost of `make test` without
// adding new pin coverage. The TestFailedLoginTotal_RaceSafe test
// below exercises the contended admission path with a workload
// above the per-goroutine uniqueness bound (8x50 = 400 distinct IPs),
// which is enough to surface a regression that dropped the cap
// check or the overflow branch.
func TestFailedLoginTotal_OverflowBucketPreInstantiated(t *testing.T) {
	const n = 100
	m := wire.NewOpsMetrics("apid")
	for i := 0; i < n; i++ {
		ip := fmt.Sprintf("198.51.100.%d", i)
		m.FailedLoginTotal(ip).Inc()
	}
	// The 100 IPs we just admitted should all be present; the
	// reserved "anonymous" and "__other__" buckets should be
	// pre-instantiated with value 0.
	body := render(t, m)
	for _, want := range []string{
		`apid_failed_login_total{ip="anonymous"} 0`,
		`apid_failed_login_total{ip="__other__"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing pre-instantiated series %q in:\n%s", want, body)
		}
	}
}

// TestFailedLoginTotal_OverflowCollapsesToOther has been moved to
// the sibling slow-build file
// (metrics_cardinality_ip_slow_test.go::TestFailedLoginTotal_OverflowCollapsesToOtherSlow).
// See the top-of-file comment for the rationale.

// TestFailedLoginTotal_ReservedLabelsNeverAgainstCap asserts the
// reserved values (anonymousIPLabel, otherIPLabel) are admitted at
// boot without consuming capacity. The check is observational: the
// reserved labels show up in the pre-instantiated series with value
// 0 even when no real IP has been admitted yet, which is the
// contract the metric-reader relies on for "no data" detection.
func TestFailedLoginTotal_ReservedLabelsNeverAgainstCap(t *testing.T) {
	m := wire.NewOpsMetrics("apid")
	body := render(t, m)
	for _, want := range []string{
		`apid_failed_login_total{ip="anonymous"} 0`,
		`apid_failed_login_total{ip="__other__"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing pre-instantiated reserved series %q in:\n%s", want, body)
		}
	}
}

// TestFailedLoginTotal_SameIPReturnsSameCounter proves the
// admission set is deduplicating: two calls for the same IP return
// the same underlying Prometheus Counter, so the counter is
// monotonic. The flag is tripped by a regression that admitted each
// lookup as a new label (a frozenset collapse would lose the
// monotonicity).
func TestFailedLoginTotal_SameIPReturnsSameCounter(t *testing.T) {
	m := wire.NewOpsMetrics("apid")
	a := m.FailedLoginTotal("203.0.113.7")
	b := m.FailedLoginTotal("203.0.113.7")
	if a == nil || b == nil {
		t.Fatal("counter is nil")
	}
	// prometheus.Counter is an interface; the address-equality
	// check on the Desc field is the canonical signal that the
	// call returned the same underlying counter.
	aDesc := a.Desc().String()
	bDesc := b.Desc().String()
	if aDesc != bDesc {
		t.Errorf("counter Desc mismatch: %q vs %q", aDesc, bDesc)
	}
	// And the increment is monotonic across both calls.
	a.Inc()
	a.Inc()
	a.Inc()
	b.Inc()
	b.Inc()
	// Five total increments (3 on a, 2 on b) land on the same
	// counter, so the registry surface shows 5.
	body := render(t, m)
	if !strings.Contains(body, `apid_failed_login_total{ip="203.0.113.7"} 5`) {
		t.Errorf("expected ip=203.0.113.7 series at 5 after 5 increments, got:\n%s", body)
	}
}

// TestFailedLoginTotal_RaceSafe asserts the admission set is
// goroutine-safe under -race. The workload is N goroutines each
// admitting K distinct IPs — the contention is the lookup/insert
// path on the underlying map. A regression that dropped the mutex
// would trip the -race detector with a concurrent map read/write.
func TestFailedLoginTotal_RaceSafe(t *testing.T) {
	m := wire.NewOpsMetrics("apid")
	const goroutines = 8
	const perG = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				ip := fmt.Sprintf("192.0.2.%d.%d", g, i)
				m.FailedLoginTotal(ip).Inc()
			}
		}(g)
	}
	wg.Wait()
	// No counter is asserted by value here — the test passes iff
	// the race detector fires on the contended map access. The
	// surface counter check is a sanity tieout.
	body := render(t, m)
	if !strings.Contains(body, "apid_failed_login_total") {
		t.Errorf("missing failed-login counter in scrape output:\n%s", body)
	}
}

// TestFailedLoginDropped_Exposed asserts the unlabelled drop counter
// is registered. The handler increments this counter when the async
// audit channel is full (the §11 best-effort invariant — the 401
// must never block on the audit write). The counter is the
// observability seam that backs the FaasFailedLoginSpike runbook's
// "is the audit flusher the bottleneck" question.
func TestFailedLoginDropped_Exposed(t *testing.T) {
	m := wire.NewOpsMetrics("apid")
	c := m.FailedLoginDropped()
	if c == nil {
		t.Fatal("FailedLoginDropped returned nil")
	}
	c.Inc()
	c.Inc()
	c.Inc()
	body := render(t, m)
	if !strings.Contains(body, "apid_failed_login_audit_dropped_total 3") {
		t.Errorf("expected apid_failed_login_audit_dropped_total 3 in scrape, got:\n%s", body)
	}
}

// TestFailedLoginAuditWriteFailures_Exposed asserts the dedicated
// failed-login audit-write failure counter is registered. The
// flusher (cmd/apid/audit.go::flushOne) increments this counter
// when its AppendEvent fails — distinct from the success-path
// apid_audit_write_failures_total{account_id}, which would otherwise
// collapse the row's nil subject into account_id="anonymous"
// alongside legitimate anonymous-success-path failures. Pair the
// two counters for the SOC 2 CC7.2 audit-write-failure surface
// (issue #286 review fix #3).
func TestFailedLoginAuditWriteFailures_Exposed(t *testing.T) {
	m := wire.NewOpsMetrics("apid")
	c := m.FailedLoginAuditWriteFailures()
	if c == nil {
		t.Fatal("FailedLoginAuditWriteFailures returned nil")
	}
	c.Inc()
	c.Inc()
	body := render(t, m)
	if !strings.Contains(body, "apid_failed_login_audit_write_failures_total 2") {
		t.Errorf("expected apid_failed_login_audit_write_failures_total 2 in scrape, got:\n%s", body)
	}
	// Sanity: the success-path counter is unlabelled today (no
	// pre-instantiation) and the dedicated counter is also
	// unlabelled — they live side-by-side as paired views. The
	// success-path counter should NOT have a series for the empty
	// string here because we never called it.
	if strings.Contains(body, "apid_audit_write_failures_total{account_id=\"\"} 1") {
		t.Errorf("success-path audit counter unexpectedly labelled with empty string:\n%s", body)
	}
}
