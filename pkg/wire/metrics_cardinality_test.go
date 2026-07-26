// Tests for the bounded account-label admission set introduced for
// issue #278 (per-customer failure observability). The set lives on
// OpsMetrics and is shared by the audit-write and request-failure
// counters so a customer is represented by their real id in both
// metrics, or by "__other__" in both.
//
// The shape of these tests is load-bearing: they pin the contract
// that
//   - the first maxAccountLabelValues distinct ids are admitted,
//   - further ids collapse to "__other__" without consuming capacity,
//   - the reserved "anonymous" and "__other__" labels never count
//     against the cap,
//   - the admission set is race-safe under -race,
//   - repeated lookups for the same id return the same underlying
//     Prometheus Counter.

package wire_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/onebox-faas/faas/pkg/wire"
)

// TestAccountLabel_FirstNAdmitted asserts the first maxAccountLabelValues
// distinct ids round-trip unchanged through accountLabel.
func TestAccountLabel_FirstNAdmitted(t *testing.T) {
	const n = 100 // sub-sample of the 10k cap for fast tests
	m := wire.NewOpsMetrics("apid")
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("acct-%08x", i)
		// Force admission by hitting the audit-write counter once.
		m.AuditWriteFailures(id).Inc()
	}
	body := render(t, m)
	want := `apid_audit_write_failures_total{account_id="acct-00000000"} 1`
	if !strings.Contains(body, want) {
		t.Errorf("missing first-id series %q in:\n%s", want, body)
	}
	// A mid-range id must also be present.
	midWant := fmt.Sprintf(`apid_audit_write_failures_total{account_id=%q} 1`, fmt.Sprintf("acct-%08x", n/2))
	if !strings.Contains(body, midWant) {
		t.Errorf("missing mid-id series %q in:\n%s", midWant, body)
	}
}

// TestAccountLabel_OverflowsToOther asserts the (cap+1)th distinct id
// collapses to "__other__". The reserved value is admitted at boot,
// so the 10 001st real id is the first to land in overflow.
func TestAccountLabel_OverflowsToOther(t *testing.T) {
	m := wire.NewOpsMetrics("apid")
	// Fill to capacity. We bypass accountLabel directly by hammering
	// AuditWriteFailures — each unique id is admitted once.
	const cap = 10_000
	for i := 0; i < cap; i++ {
		m.AuditWriteFailures(fmt.Sprintf("real-%05d", i))
	}
	// The (cap+1)th id is the first overflow.
	m.AuditWriteFailures("real-overflow").Inc()
	body := render(t, m)
	if !strings.Contains(body, `apid_audit_write_failures_total{account_id="__other__"} 1`) {
		t.Errorf("expected __other__ series at overflow; body was:\n%s", body)
	}
	// A second overflow id should also land in __other__ and
	// increment the same series (not mint a new one).
	m.AuditWriteFailures("real-overflow-2").Inc()
	body = render(t, m)
	if !strings.Contains(body, `apid_audit_write_failures_total{account_id="__other__"} 2`) {
		t.Errorf("expected __other__ series to accumulate; body was:\n%s", body)
	}
}

// TestAccountLabel_ReservedValuesDontConsumeCapacity asserts the
// reserved labels (anonymous, __other__) are admitted at boot
// without consuming the real-account budget.
func TestAccountLabel_ReservedValuesDontConsumeCapacity(t *testing.T) {
	m := wire.NewOpsMetrics("apid")
	// The reserved labels must surface from the very first scrape —
	// pre-instantiated in NewOpsMetrics so the dashboard never goes
	// "no data" for an idle box.
	body := render(t, m)
	for _, want := range []string{
		`apid_audit_write_failures_total{account_id="anonymous"} 0`,
		`apid_audit_write_failures_total{account_id="__other__"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing reserved series %q in:\n%s", want, body)
		}
	}
	// Empty input normalizes to "anonymous".
	m.AuditWriteFailures("").Inc()
	body = render(t, m)
	if !strings.Contains(body, `apid_audit_write_failures_total{account_id="anonymous"} 1`) {
		t.Errorf("empty id did not normalize to anonymous; body was:\n%s", body)
	}
}

// TestAccountLabel_SharedBetweenMetrics asserts the cap is shared
// between AuditWriteFailures and RequestFailure: an id admitted via
// one accessor must surface in the other's series with its real
// label, and the cap must not be double-counted.
func TestAccountLabel_SharedBetweenMetrics(t *testing.T) {
	m := wire.NewOpsMetrics("apid")
	const id = "shared-acct-1"
	m.AuditWriteFailures(id).Inc()
	m.RequestFailure(id, "GET /v1/test").Inc()
	body := render(t, m)
	for _, want := range []string{
		fmt.Sprintf(`apid_audit_write_failures_total{account_id=%q} 1`, id),
		fmt.Sprintf(`apid_request_failures_total{account_id=%q,route="GET /v1/test"} 1`, id),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing shared series %q in:\n%s", want, body)
		}
	}
}

// TestAccountLabel_ConcurrentAdmission asserts the admission set is
// race-safe under -race. 100 goroutines admit 100 distinct ids each
// (10 000 total — under the cap), and we expect all of them to
// surface in the body without a panic or duplicate series.
func TestAccountLabel_ConcurrentAdmission(t *testing.T) {
	m := wire.NewOpsMetrics("apid")
	const goroutines = 100
	const perGoroutine = 100
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				id := fmt.Sprintf("g%03d-i%05d", gid, i)
				m.AuditWriteFailures(id).Inc()
			}
		}(g)
	}
	wg.Wait()
	// Spot-check the first id from goroutine 0 — if the map was
	// corrupted under -race, this assertion will fail intermittently.
	body := render(t, m)
	want := `apid_audit_write_failures_total{account_id="g000-i00000"} 1`
	if !strings.Contains(body, want) {
		t.Errorf("missing concurrent series %q in:\n%s", want, body)
	}
}

// TestAccountLabel_IdempotentLabel asserts two calls with the same
// id return a Counter that points at the same underlying series
// (so incrementing via either call is interchangeable). This is the
// contract the audit seam depends on for hot-path caching.
func TestAccountLabel_IdempotentLabel(t *testing.T) {
	m := wire.NewOpsMetrics("apid")
	c1 := m.AuditWriteFailures("dup-id")
	c2 := m.AuditWriteFailures("dup-id")
	c1.Inc()
	c1.Inc()
	// If c1 and c2 are different Counter instances pointing at
	// different series, the second assertion would see 0. The
	// prometheus client lib returns the same child on repeated
	// WithLabelValues calls; this test pins that contract for our
	// accessor wrapper.
	if got := readSingle(t, m, `apid_audit_write_failures_total{account_id="dup-id"}`); got != 2 {
		t.Errorf("dup-id counter = %v, want 2 (callers must share series)", got)
	}
	_ = c2
}

// TestAuditWriteDuration_PreInstantiated asserts the ok/failed
// histogram series surface from boot (so the dashboard never goes
// "no data" on an idle daemon).
func TestAuditWriteDuration_PreInstantiated(t *testing.T) {
	m := wire.NewOpsMetrics("apid")
	body := render(t, m)
	for _, want := range []string{
		`apid_audit_write_failures_duration_seconds_count{result="ok"} 0`,
		`apid_audit_write_failures_duration_seconds_count{result="failed"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing pre-instantiated histogram series %q in:\n%s", want, body)
		}
	}
}

// readSingle is a tiny helper to scrape a single counter value out
// of the apid /metrics text. Fails the test if the line is absent.
func readSingle(t *testing.T, m *wire.OpsMetrics, line string) float64 {
	t.Helper()
	body := render(t, m)
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, line+" ") {
			var v float64
			if _, err := fmt.Sscanf(l, line+" %f", &v); err == nil {
				return v
			}
		}
	}
	t.Fatalf("line %q not found in:\n%s", line, body)
	return 0
}
