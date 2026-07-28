// Slow-build sibling of metrics_cardinality_ip_test.go. The 10_000-IP
// overflow-collapse driver is gated behind //go:build slow so the
// default `make test` loop stays fast (the per-IP counter registry
// scrape is the dominant cost; 100 IPs in the fast test already
// exercise the lookup/insert path). The slow build pins the
// production cardinality cap exactly.
//
// Run with:  go test -tags=slow ./pkg/wire/...
//
//go:build slow

package wire_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/wire"
)

// TestFailedLoginTotal_OverflowCollapsesToOtherSlow exercises the
// load-bearing cardinality contract. The counter admits
// maxIPLabelValues distinct IPs and any further IP collapses to
// ip="__other__" instead of minting a new series.
//
// The reserved-label accounting was fixed in this PR: the previous
// behavior stole two slots from the real-IP budget (the
// pre-seeded reserved labels counted toward the cap), so the
// 9999th new IP silently collapsed to __other__ instead of the
// 10001th. The shipped `ipBudget = 10_000` here is the documented
// "max distinct IPs" budget; the admission set's underlying map
// holds 10_002 entries (10_000 real IPs + 2 reserved) when the set
// fills.
//
// On the default test path (no -tags=slow), this file is compiled
// out — see the build tag at the top.
func TestFailedLoginTotal_OverflowCollapsesToOtherSlow(t *testing.T) {
	const ipBudget = 10_000
	m := wire.NewOpsMetrics("apid")
	// Drive ipBudget distinct IPs through the admission set.
	for i := 0; i < ipBudget; i++ {
		// 10_000 < 256^2, so two octets give distinct values.
		ip := fmt.Sprintf("203.0.%d.%d", (i/256)%256, i%256)
		m.FailedLoginTotal(ip).Inc()
	}
	body := render(t, m)
	// Diagnostic: count substring matches. Each literal-IP series
	// contributes one match (the `apid_failed_login_total{ip="..."}`
	// line); `__other__` contributes one. If overflow is working,
	// all 10_000 distinct driver IPs admit (10_000 matches for
	// `203.0.`) and the next distinct IPs collapse to a single
	// `__other__` series.
	cLiteral := strings.Count(body, `apid_failed_login_total{ip="203.0.`)
	cOther := strings.Count(body, `apid_failed_login_total{ip="__other__"} 1`)
	t.Logf("literal-IP series: %d, __other__=1 series: %d", cLiteral, cOther)
	if cLiteral != 10_000 {
		t.Fatalf("expected 10_000 literal-IP series, got %d (overflow happened too early or admission leaked)", cLiteral)
	}
	// The next two fresh IPs (i=10000 and i=10001 — already past
	// the budget) MUST collapse to __other__.
	m.FailedLoginTotal("198.51.100.250").Inc()
	m.FailedLoginTotal("198.51.100.251").Inc()
	body = render(t, m)
	cOtherAfter := strings.Count(body, `apid_failed_login_total{ip="__other__"}`)
	if cOtherAfter != 1 {
		t.Errorf("expected 1 __other__ series after overflow, got %d (overflow collapses failing)", cOtherAfter)
	}
	// And the two overflow IPs must NOT appear as literal labels.
	for _, overflow := range []string{"198.51.100.250", "198.51.100.251"} {
		if strings.Contains(body, fmt.Sprintf(`apid_failed_login_total{ip=%q}`, overflow)) {
			t.Errorf("overflow IP %q leaked into the scrape as a literal label", overflow)
		}
	}
	// __other__ value should be 2 (the two overflow Inc calls).
	if !strings.Contains(body, `apid_failed_login_total{ip="__other__"} 2`) {
		t.Errorf("expected __other__ value 2 after two overflow Inc calls, scrape:\n%s", body)
	}
}
