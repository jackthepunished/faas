// Tests for the vmmd-side per-app wake-failure split (issue
// #1059 / ADR-127 §3.5 — cluster A commit 4 of the
// platform-observability mega-PR). The 6 vmmd hook sites at
// pkg/fcvm/manager.go (setupNetwork, plan cgroup, workload
// cgroup, restore-fallback, cold-boot terminal) thread
// `req.AppID` through the WakeFailure accessor so the per-app
// wake-failure breakdown lands on vmmd_wake_failure_total{app=...}.
//
// This file exercises the metric surface only — the Manager.Wake
// harness integration is covered by the `make test-metal` suite
// where a fake jailer / fake netns is wired in. The metric-only
// pin here is the regression guard for "did the per-app split
// actually surface as distinct (box, app, reason) series on the
// vmmd-side registry".

package fcvm_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/wire"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestWakeFailure_HookSites_AppSplit pins the per-app wake-
// failure split contract at the vmmd-side hook sites (issue
// #1059 / ADR-127 §3.5). Each of the 6 hook sites in
// pkg/fcvm/manager.go now calls:
//
//	m.wakeFailureMetrics.WakeFailure("local", req.AppID, <reason>).Inc()
//
// The test fires the metric the way the hook sites do, with two
// distinct app IDs ("app-alpha" and "app-beta"), and asserts:
//
//   - the two app IDs surface as distinct series in the scrape
//     body (the per-app split is observable),
//   - the underlying Prometheus counters are dedup keys —
//     repeated Inc on the same (box, app, reason) tuple is
//     monotonic, NOT a frozenset collapse,
//   - the labelAppUnknown bucket (app="") stays at 0 unless
//     explicitly bumped (the gRPC parse-failure path is the
//     only call site that emits "" today — pkg/vmmdgrpc/server.go).
func TestWakeFailure_HookSites_AppSplit(t *testing.T) {
	ops := wire.NewOpsMetrics("vmmd")

	// Drive the metric the way the hook sites do — two distinct
	// apps, two distinct reasons, plus one reason shared between
	// the two apps. The shared reason verifies that the (box,
	// app, reason) tuple is the dedup key — a different app's
	// counter increment must NOT bump the first app's counter.
	alpha := ops.WakeFailure("local", "app-alpha", "snapshot_restore_err")
	alpha.Inc()
	alpha.Inc()
	beta := ops.WakeFailure("local", "app-beta", "snapshot_restore_err")
	beta.Inc()
	ops.WakeFailure("local", "app-alpha", "jailer_fail").Inc()

	// Counter monotonicity: alpha at 2, beta at 1.
	if got := testutil.ToFloat64(alpha); got != 2 {
		t.Errorf("app-alpha counter = %v, want 2", got)
	}
	if got := testutil.ToFloat64(beta); got != 1 {
		t.Errorf("app-beta counter = %v, want 1", got)
	}

	// Scrape-body shape — the (box, app, reason) tuples surface
	// as distinct series, NOT collapsed.
	body := scrapeOpsMetrics(t, ops)
	for _, want := range []string{
		`vmmd_wake_failure_total{app="app-alpha",box="local",reason="snapshot_restore_err"} 2`,
		`vmmd_wake_failure_total{app="app-beta",box="local",reason="snapshot_restore_err"} 1`,
		`vmmd_wake_failure_total{app="app-alpha",box="local",reason="jailer_fail"} 1`,
		// labelAppUnknown bucket (app="") stays at 0 — the hook
		// sites thread req.AppID today; only the gRPC
		// parse-failure path emits "".
		`vmmd_wake_failure_total{app="",box="local",reason="snapshot_restore_err"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
}

// TestWakeFailure_HookSites_OverflowCollapsesPerApp pins the
// per-app overflow behaviour. The appLabelSet admission
// (maxAppLabelValues = 256) collapses overflow to otherAppLabel
// — but the collapse happens per-app, not globally. A 257th
// distinct app still has a row at app="__other__" while the
// first 256 distinct apps retain their real-app series.
//
// This is the regression guard for a fix that would conflate
// "real app that hit the admission cap" with "real app not yet
// admitted" — operators triaging the per-app panel must be able
// to tell "this app is one of the 256 fleet-known apps" from
// "this app is an overflow bucket".
func TestWakeFailure_HookSites_OverflowCollapsesPerApp(t *testing.T) {
	ops := wire.NewOpsMetrics("vmmd")

	// Admit 256 distinct real apps on the snapshot_restore_err
	// reason — the hook sites will fire this reason on a
	// restore-fallback.
	for i := 0; i < 256; i++ {
		ops.WakeFailure("local", fmt.Sprintf("app-%03d", i), "snapshot_restore_err").Inc()
	}
	// Drive the 257th distinct app — must collapse to
	// otherAppLabel.
	overflow := ops.WakeFailure("local", "overflow-app", "snapshot_restore_err")
	overflow.Inc()

	body := scrapeOpsMetrics(t, ops)
	// The 256 admitted apps each surface at 1.
	for i := 0; i < 256; i++ {
		want := fmt.Sprintf(`vmmd_wake_failure_total{app="app-%03d",box="local",reason="snapshot_restore_err"} 1`, i)
		if !strings.Contains(body, want) {
			t.Errorf("missing admitted series %q in:\n%s", want, body)
		}
	}
	// The overflow app must collapse to otherAppLabel — the
	// literal "overflow-app" label must NOT appear.
	if strings.Contains(body, `vmmd_wake_failure_total{app="overflow-app",box="local",reason="snapshot_restore_err"}`) {
		t.Errorf("overflow-app unexpectedly admitted (should collapse to __other__):\n%s", body)
	}
	// And the __other__ bucket must surface at 1.
	if !strings.Contains(body, `vmmd_wake_failure_total{app="__other__",box="local",reason="snapshot_restore_err"} 1`) {
		t.Errorf("missing __other__ bucket series in:\n%s", body)
	}
}

// TestWakeFailure_HookSites_AllReasonsPresent asserts every
// vmmd-side hook-site reason is reachable from the metric
// surface — a regression that drops a constant from the closed
// vocab would leave the corresponding series missing from
// /metrics at boot, and a wake-failure spike for that reason
// would not surface in the dashboard panel.
func TestWakeFailure_HookSites_AllReasonsPresent(t *testing.T) {
	ops := wire.NewOpsMetrics("vmmd")
	body := scrapeOpsMetrics(t, ops)
	// The 8 vmmd-side closed-vocab reasons (from
	// pkg/fcvm/wake_classify.go::WakeReason* constants).
	// Each maps to a hook site in pkg/fcvm/manager.go — see
	// ADR-127 §3 for the full mapping table.
	for _, reason := range []string{
		"snapshot_stale", "disk_full", "jailer_fail", "netns_fail",
		"cgroup_fail", "vsock_fail", "snapshot_restore_err", "mem_backend_err",
	} {
		want := `vmmd_wake_failure_total{app="",box="local",reason="` + reason + `"} 0`
		if !strings.Contains(body, want) {
			t.Errorf("missing pre-instantiated closed-vocab series for reason=%q in:\n%s", reason, body)
		}
	}
}

// scrapeOpsMetrics fetches the /metrics scrape body for the
// given OpsMetrics registry. Mirrors the helpers in
// pkg/wire/metrics_test.go::render and pkg/sched/engine_test.go
// ::getMetricsBody — duplicated here because the fcvm test
// package cannot import the wire test-helper file (internal-only).
func scrapeOpsMetrics(t *testing.T, ops *wire.OpsMetrics) string {
	t.Helper()
	srv := httptest.NewServer(ops.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}
