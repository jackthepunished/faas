// Tests for the OpsMetrics helper and the /metrics handler. We exercise:
//   - counter incremented per Observe call, labelled by op + code
//   - histogram observes per Observe call, labelled by op
//   - code label is "ok" on success and "err" on non-nil error
//   - the HTTP handler emits both series in the Prometheus text format

package wire_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/wire"
)

func TestOpsMetrics_ObserveCounter(t *testing.T) {
	m := wire.NewOpsMetrics("vmmd")
	m.Observe("CreateFromSnapshot", 12*time.Millisecond, nil)
	m.Observe("CreateFromSnapshot", 10*time.Millisecond, nil)
	m.Observe("Stats", 200*time.Microsecond, errors.New("boom"))

	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	body := string(buf)

	for _, want := range []string{
		`vmmd_ops_total{code="ok",op="CreateFromSnapshot"} 2`,
		`vmmd_ops_total{code="err",op="Stats"} 1`,
		`vmmd_op_duration_seconds_count{op="CreateFromSnapshot"} 2`,
		`vmmd_op_duration_seconds_count{op="Stats"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing line %q in:\n%s", want, body)
		}
	}
}

func TestOpsMetrics_IndependentRegistries(t *testing.T) {
	// Two daemons must not collide if they construct in the same process —
	// that's the point of per-daemon Registry over the global default.
	a := wire.NewOpsMetrics("vmmd")
	b := wire.NewOpsMetrics("builderd")
	a.Observe("ColdBoot", time.Millisecond, nil)
	b.Observe("Build", 50*time.Millisecond, nil)

	// vmmd's endpoint must NOT mention builderd series, and vice versa.
	bodyA := render(t, a)
	bodyB := render(t, b)

	if !strings.Contains(bodyA, `vmmd_ops_total{code="ok",op="ColdBoot"} 1`) {
		t.Errorf("vmmd endpoint missing vmmd series:\n%s", bodyA)
	}
	if strings.Contains(bodyA, "builderd_") {
		t.Errorf("vmmd endpoint leaked builderd:\n%s", bodyA)
	}
	if !strings.Contains(bodyB, `builderd_ops_total{code="ok",op="Build"} 1`) {
		t.Errorf("builderd endpoint missing builderd series:\n%s", bodyB)
	}
	if strings.Contains(bodyB, "vmmd_") {
		t.Errorf("builderd endpoint leaked vmmd:\n%s", bodyB)
	}
}

func render(t *testing.T, m *wire.OpsMetrics) string {
	t.Helper()
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()
	body, err := readAll(t, srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return body
}

func TestOpsMetrics_ObserveBuild(t *testing.T) {
	m := wire.NewOpsMetrics("builderd")
	m.ObserveBuildCount("ok")
	m.ObserveBuildCount("ok")
	m.ObserveBuildCount("cache_hit")
	m.ObserveBuildCount("user_error")
	m.ObserveBuildDuration("ok", 42*time.Second)
	m.ObserveBuildDuration("cache_hit", 200*time.Millisecond)
	m.ObserveBuildQueueWait(3 * time.Second)

	body := render(t, m)
	for _, want := range []string{
		`builderd_ops_total{code="ok",op="build"} 2`,
		`builderd_ops_total{code="cache_hit",op="build"} 1`,
		`builderd_ops_total{code="user_error",op="build"} 1`,
		`builderd_build_duration_seconds_count{outcome="ok"} 1`,
		`builderd_build_duration_seconds_count{outcome="cache_hit"} 1`,
		`builderd_build_duration_seconds_count{outcome="failed"} 0`,
		`builderd_build_queue_wait_seconds_count 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing line %q in:\n%s", want, body)
		}
	}
}

func TestOpsMetrics_ObserveBuildNilSafe(t *testing.T) {
	// builderd unit tests construct the orchestrator without metrics; the
	// observers must be no-ops on a nil receiver rather than panicking.
	var m *wire.OpsMetrics
	m.ObserveBuildCount("ok")
	m.ObserveBuildDuration("ok", time.Second)
	m.ObserveBuildQueueWait(time.Second)
}

func TestOpsMetrics_ObserveImagedOCIPull(t *testing.T) {
	// Same shape as the vmm-proxy test: real observations on a subset of
	// (op, result) tuples + zero-valued pre-instantiated buckets for the
	// rest of the closed set.
	m := wire.NewOpsMetrics("imaged")
	m.ObserveImagedOCIPull("manifest", "ok", 200*time.Millisecond)
	m.ObserveImagedOCIPull("blob", "ok", 5*time.Second)
	m.ObserveImagedOCIPull("blob", "err", 60*time.Second)
	m.ObserveImagedOCIPull("above_base", "ok", 800*time.Millisecond)

	body := render(t, m)
	for _, want := range []string{
		// Real observations.
		`imaged_oci_pull_duration_seconds_count{op="manifest",result="ok"} 1`,
		`imaged_oci_pull_duration_seconds_count{op="blob",result="ok"} 1`,
		`imaged_oci_pull_duration_seconds_count{op="blob",result="err"} 1`,
		`imaged_oci_pull_duration_seconds_count{op="above_base",result="ok"} 1`,
		// Pre-instantiated tuples we never observed: still zero-valued.
		`imaged_oci_pull_duration_seconds_count{op="config",result="ok"} 0`,
		`imaged_oci_pull_duration_seconds_count{op="manifest",result="err"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing line %q in:\n%s", want, body)
		}
	}
}

func TestOpsMetrics_NewObserversNilSafe(t *testing.T) {
	// imaged unit tests construct the orchestrator without metrics;
	// the new observer must be a no-op on a nil receiver.
	var m *wire.OpsMetrics
	m.ObserveImagedOCIPull("blob", "ok", time.Second)
}

// TestOpsMetrics_PgBackupLastPushed (issue #250) — the
// pg_backup_last_pushed_seconds gauge must:
//   - be registered under the per-daemon prefix,
//   - surface in /metrics from boot pre-instantiated to 0,
//   - accept Set() calls without panicking,
//   - return nil from the accessor on a nil receiver (nil-safe).
//
// The PgBackupStale alert rule (deploy/ansible/roles/prometheus/files/pg_backup.rules.yml)
// queries `time() - pg_backup_last_pushed_seconds > 86400`; without
// the gauge series from boot, a freshly-booted box looks identical
// to one with no basebackup root — both return NaN to the alert,
// and the alert is silently skipped. The pre-instantiated-to-0
// pattern (mirror of alertEvaluatorEnabled, line ~771) closes the
// gap.
func TestOpsMetrics_PgBackupLastPushed(t *testing.T) {
	m := wire.NewOpsMetrics("apid")
	body := render(t, m)
	want := `apid_pg_backup_last_pushed_seconds 0`
	if !strings.Contains(body, want) {
		t.Errorf("missing line %q in:\n%s", want, body)
	}
	// Set must not panic; the gauge accepts arbitrary float64 values
	// (apid's sampler writes time.Since(newest).Seconds()).
	m.PgBackupLastPushed().Set(3600)
	body = render(t, m)
	if !strings.Contains(body, `apid_pg_backup_last_pushed_seconds 3600`) {
		t.Errorf("gauge did not surface Set value:\n%s", body)
	}
}

// TestOpsMetrics_PgBackupLastPushedNilSafe — nil-receiver accessor.
func TestOpsMetrics_PgBackupLastPushedNilSafe(t *testing.T) {
	var m *wire.OpsMetrics
	if g := m.PgBackupLastPushed(); g != nil {
		t.Errorf("nil receiver returned non-nil gauge: %v", g)
	}
}

// TestOpsMetrics_StandbyStatePreinstantiated — Tier A8 / ADR-083
// standby-state enum gauge must surface in /metrics from boot
// (precedent: alertEvaluatorEnabled at metrics.go:771-779 and
// pgBackupLastPushed above). The FaasStandbyStateWarmingTooLong
// alert queries this gauge; a missing series surfaces as "no
// data" rather than 0, which the alert rule would misread.
func TestOpsMetrics_StandbyStatePreinstantiated(t *testing.T) {
	m := wire.NewOpsMetrics("gatewayd_public")
	body := render(t, m)
	want := `gatewayd_public_standby_state 1`
	if !strings.Contains(body, want) {
		t.Errorf("missing line %q in:\n%s", want, body)
	}
	if got := m.StandbyState(); got != 1 {
		t.Errorf("StandbyState() = %d, want 1 (warming)", got)
	}
	// Set must round-trip through both the gauge AND the shadow
	// value (the latter is what callers read without scraping
	// /metrics; precedent: alertEvaluatorEnabledValue).
	m.SetStandbyState(2) // warm
	body = render(t, m)
	if !strings.Contains(body, `gatewayd_public_standby_state 2`) {
		t.Errorf("gauge did not surface Set(2) value:\n%s", body)
	}
	if got := m.StandbyState(); got != 2 {
		t.Errorf("StandbyState() after Set(2) = %d, want 2", got)
	}
	m.SetStandbyState(3) // draining
	if got := m.StandbyState(); got != 3 {
		t.Errorf("StandbyState() after Set(3) = %d, want 3", got)
	}
}

// TestOpsMetrics_ActivePassiveFailoversPreinstantiated — Tier A8
// / ADR-083 active-passive fail-over counter must surface every
// closed outcome label in /metrics from boot (precedent:
// liveMigrationDecisions / migratingReconcileDecisions). Operators
// rely on the panel existing at day 1 — an idle box would
// otherwise render the panel as "no data" until the first drain.
func TestOpsMetrics_ActivePassiveFailoversPreinstantiated(t *testing.T) {
	m := wire.NewOpsMetrics("gatewayd_public")
	body := render(t, m)
	for _, outcome := range []string{
		"dns_flipped", "dns_stale", "peer_unreachable", "manual_drain",
	} {
		want := fmt.Sprintf("gatewayd_public_active_passive_failovers_total{outcome=%q} 0", outcome)
		if !strings.Contains(body, want) {
			t.Errorf("missing pre-instantiated label %q in:\n%s", want, body)
		}
	}
	// Inc must round-trip; the dns_flipped label is the §12
	// dashboard panel (sum over 5m for the rate).
	m.ActivePassiveFailovers("dns_flipped").Inc()
	body = render(t, m)
	if !strings.Contains(body, `gatewayd_public_active_passive_failovers_total{outcome="dns_flipped"} 1`) {
		t.Errorf("Inc did not surface value:\n%s", body)
	}
}

// TestOpsMetrics_WriteRedirectPreinstantiated — Tier A9 / ADR-084
// write-redirect counter must surface every (outcome, auth_kind)
// label combination in /metrics from boot. The §12 dashboard
// relies on the panel existing at day 1 — an idle box would
// otherwise render the panel as "no data" until the first
// standby write. The label vocabulary is closed (8 outcomes ×
// 3 auth_kinds = 24 series), imported from pkg/gateway/writegate
// (review finding #5 of PR #761) so a drift between the writeGate
// outcome vocabulary and the metric pre-instantiation breaks the
// compile rather than the dashboard.
func TestOpsMetrics_WriteRedirectPreinstantiated(t *testing.T) {
	m := wire.NewOpsMetrics("gatewayd_internal")
	body := render(t, m)
	outcomes := []string{
		"relayed", "redirect_307", "same_box", "cookie_blocked",
		"leader_unreachable", "loop_prevented", "mTLS_failure", "error",
	}
	authKinds := []string{"bearer", "cookie", "anonymous"}
	for _, outcome := range outcomes {
		for _, kind := range authKinds {
			// Prometheus emits labels in alphabetical order,
			// so auth_kind comes before outcome in the wire form.
			want := fmt.Sprintf(
				`gatewayd_internal_write_redirect_total{auth_kind=%q,outcome=%q} 0`,
				kind, outcome)
			if !strings.Contains(body, want) {
				t.Errorf("missing pre-instantiated label %s in:\n%s", want, body)
			}
		}
	}
	// Round-trip: Inc on the bearer+relayed counter must surface.
	m.WriteRedirectTotal("relayed", "bearer").Inc()
	body = render(t, m)
	want := `gatewayd_internal_write_redirect_total{auth_kind="bearer",outcome="relayed"} 1`
	if !strings.Contains(body, want) {
		t.Errorf("Inc did not surface value:\n%s", body)
	}
	// Histogram accessor must be non-nil and Observe must succeed.
	if m.WriteRedirectLatency() == nil {
		t.Fatalf("WriteRedirectLatency() = nil (histogram not registered)")
	}
	m.WriteRedirectLatency().Observe(0.123)
}

// TestOpsMetrics_EgressDenyRegistryPreinstantiated (PR-E) — every
// catalog (cidr, family) tuple must surface in /metrics from boot
// with value 0, mirroring the OCI-pull and build histogram
// pre-instantiation pattern. The wire test pins both the vmmd-side
// (prefix "vmmd") collector and the imaged-side (prefix "imaged")
// OCI mirror collector. Operators rely on the panel existing at
// day 1 — an idle box would otherwise render the panel as "no
// data" until at least one drop had been observed.
func TestOpsMetrics_EgressDenyRegistryPreinstantiated(t *testing.T) {
	cases := []struct {
		prefix      string
		wantCatalog []string
		wantOCIOnly []string // only on "imaged"
	}{
		{
			prefix: "vmmd",
			// Catalog pre-instantiation covers the firewall-side counter.
			wantCatalog: []string{
				`vmmd_egress_deny_total{cidr="drop_v4_10_0_0_0_8",family="ip"} 0`,
				`vmmd_egress_deny_total{cidr="drop_v6_fe80___10",family="ip6"} 0`,
				`vmmd_egress_deny_total{cidr="drop_v6_2002___16",family="ip6"} 0`,
			},
		},
		{
			prefix: "imaged",
			// Imaged registry also gets the OCI-mirror collector with
			// the catalog portion pre-instantiated (OCI-only extras are
			// pre-instantiated from cmd/imaged/main.go so wire doesn't
			// import pkg/oci).
			wantCatalog: []string{
				`imaged_egress_deny_total{cidr="drop_v4_10_0_0_0_8",family="ip"} 0`,
				`imaged_oci_egress_deny_total{cidr="drop_v4_10_0_0_0_8",family="ip"} 0`,
			},
		},
	}
	for _, c := range cases {
		t.Run(c.prefix, func(t *testing.T) {
			m := wire.NewOpsMetrics(c.prefix)
			body := render(t, m)
			for _, want := range c.wantCatalog {
				if !strings.Contains(body, want) {
					t.Errorf("missing line %q in:\n%s", want, body)
				}
			}
		})
	}
}

// TestOpsMetrics_EgressDenyIncrement (PR-E) — the public
// EgressDeny accessor increments the per-(cidr, family) counter
// and the value surfaces in /metrics. Asserts the wire path
// end-to-end (the cmd/vmmd poller + cmd/imaged hook both rely on
// this).
func TestOpsMetrics_EgressDenyIncrement(t *testing.T) {
	m := wire.NewOpsMetrics("vmmd")
	m.EgressDeny("drop_v4_10_0_0_0_8", "ip").Add(7)
	body := render(t, m)
	want := `vmmd_egress_deny_total{cidr="drop_v4_10_0_0_0_8",family="ip"} 7`
	if !strings.Contains(body, want) {
		t.Errorf("missing line %q in:\n%s", want, body)
	}
}

// TestOpsMetrics_OCIEgressDenyIncrement (PR-E) — the imaged-side
// mirror. Only registered when prefix == "imaged"; nil-safe
// accessor on every other prefix.
func TestOpsMetrics_OCIEgressDenyIncrement(t *testing.T) {
	m := wire.NewOpsMetrics("imaged")
	m.OCIEgressDeny("drop_v4_10_0_0_0_8", "ip").Inc()
	body := render(t, m)
	want := `imaged_oci_egress_deny_total{cidr="drop_v4_10_0_0_0_8",family="ip"} 1`
	if !strings.Contains(body, want) {
		t.Errorf("missing line %q in:\n%s", want, body)
	}

	// Non-imaged registries: the accessor returns nil (a no-op counter).
	// The wire contract — cmd/vmmd only ever calls EgressDeny, cmd/imaged
	// only ever calls OCIEgressDeny — is enforced by the prefix check in
	// NewOpsMetrics, not by the accessor. Verify the accessor is safe
	// on a non-imaged registry (no panic, no /metrics line).
	vmmd := wire.NewOpsMetrics("vmmd")
	if c := vmmd.OCIEgressDeny("drop_v4_10_0_0_0_8", "ip"); c != nil {
		t.Errorf("vmmd.OCIEgressDeny = %v, want nil", c)
	}
	vmmdBody := render(t, vmmd)
	// The vmmd registry must NOT have a counter NAMED oci_egress_deny_total
	// (only the firewall-side vmmd_egress_deny_total). The HELP text for
	// vmmd_egress_deny_total mentions "oci_egress_deny_total" in its
	// description; substring match would falsely trip on that. Anchor
	// the check on the metric-name declaration line.
	if strings.Contains(vmmdBody, "# TYPE vmmd_oci_egress_deny_total counter") {
		t.Errorf("vmmd registry should not contain vmmd_oci_egress_deny_total, got:\n%s", vmmdBody)
	}
}

// TestOpsMetrics_EgressDenyNilSafe (PR-E) — the accessor must be
// no-op on a nil receiver so vmmd / imaged unit tests without
// metrics keep working (same nil-safe posture as Observe*).
func TestOpsMetrics_EgressDenyNilSafe(t *testing.T) {
	var m *wire.OpsMetrics
	if got := m.EgressDeny("drop_v4_10_0_0_0_8", "ip"); got != nil {
		t.Errorf("nil.EgressDeny = %v, want nil", got)
	}
	if got := m.OCIEgressDeny("drop_v4_10_0_0_0_8", "ip"); got != nil {
		t.Errorf("nil.OCIEgressDeny = %v, want nil", got)
	}
}

// TestOpsMetrics_ObserveScaleDown (issue #171) — the aggressive-
// reaper observer increments the per-(app, outcome) counter and the
// value surfaces in /metrics. Pins the wire path end-to-end. Two
// `park` observations on `app="a1"` + one `keep` + pre-instantiated
// empty-app placeholders. Mirrors TestOpsMetrics_ObserveBuild.
func TestOpsMetrics_ObserveScaleDown(t *testing.T) {
	m := wire.NewOpsMetrics("schedd")
	m.ObserveScaleDown("a1", "park")
	m.ObserveScaleDown("a1", "park")
	m.ObserveScaleDown("a1", "keep")
	m.ObserveScaleDown("a1", "cooldown_held")

	body := render(t, m)
	for _, want := range []string{
		// Real observations.
		`schedd_scale_down_decisions_total{app="a1",outcome="park"} 2`,
		`schedd_scale_down_decisions_total{app="a1",outcome="keep"} 1`,
		`schedd_scale_down_decisions_total{app="a1",outcome="cooldown_held"} 1`,
		// Pre-instantiated empty-app placeholder: zero-valued, must
		// surface in /metrics from boot so the panel exists at day 1.
		// min_floor_already (PR-C, issue #462) is pre-instantiated
		// alongside park / keep so the closed outcome label set
		// is fully surfaced from boot. cooldown_held (P1C) is the
		// per-app scale-in cooldown consult in ReapAggressive
		// (pkg/sched/reaper.go) — pre-instantiated so dashboards
		// panel-query `outcome="cooldown_held"` returns 0 rather
		// than a missing series on an idle box.
		`schedd_scale_down_decisions_total{app="",outcome="park"} 0`,
		`schedd_scale_down_decisions_total{app="",outcome="keep"} 0`,
		`schedd_scale_down_decisions_total{app="",outcome="min_floor_already"} 0`,
		`schedd_scale_down_decisions_total{app="",outcome="cooldown_held"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing line %q in:\n%s", want, body)
		}
	}
}

// TestOpsMetrics_ObserveScaleDownNilSafe — schedd unit tests
// construct the engine without metrics; the observer must be a
// no-op on a nil receiver rather than panicking.
func TestOpsMetrics_ObserveScaleDownNilSafe(t *testing.T) {
	var m *wire.OpsMetrics
	m.ObserveScaleDown("a1", "park")
}

// TestOpsMetrics_ObserveScaleUpClosedSet (PR-C, issue #462) —
// pins the scale-up outcome label set: pre-instantiated rows
// for the closed set {admit, reject_at_cap, no_signal,
// cooldown_held} must surface in /metrics from boot with the
// empty-app placeholder. Mirrors TestOpsMetrics_ObserveScaleDown
// for the scale-up side. The cooldown_held outcome is the new
// wake-gate path emission added by PR-C.
func TestOpsMetrics_ObserveScaleUpClosedSet(t *testing.T) {
	m := wire.NewOpsMetrics("schedd")
	m.ObserveScaleUp("a1", "admit")
	m.ObserveScaleUp("a1", "cooldown_held")
	m.ObserveScaleUp("a1", "min_floor_already")
	m.ObserveScaleUp("a1", "overage_cap_reached")

	body := render(t, m)
	for _, want := range []string{
		// Real observations.
		`schedd_scale_up_decisions_total{app="a1",outcome="admit"} 1`,
		`schedd_scale_up_decisions_total{app="a1",outcome="cooldown_held"} 1`,
		`schedd_scale_up_decisions_total{app="a1",outcome="min_floor_already"} 1`,
		`schedd_scale_up_decisions_total{app="a1",outcome="overage_cap_reached"} 1`,
		// Pre-instantiated empty-app placeholder for the closed set.
		// P1A: the closed set is now 6 outcomes — min_floor_already
		// (engine.go:4868-4873) and overage_cap_reached (issue #561,
		// engine.go:4876-4888) joined the four pre-existing values.
		// The closed-set loop must pre-instantiate all six so the
		// §12 panel-at-day-1 contract holds (PR #826 precedent).
		`schedd_scale_up_decisions_total{app="",outcome="admit"} 0`,
		`schedd_scale_up_decisions_total{app="",outcome="reject_at_cap"} 0`,
		`schedd_scale_up_decisions_total{app="",outcome="no_signal"} 0`,
		`schedd_scale_up_decisions_total{app="",outcome="cooldown_held"} 0`,
		`schedd_scale_up_decisions_total{app="",outcome="min_floor_already"} 0`,
		`schedd_scale_up_decisions_total{app="",outcome="overage_cap_reached"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing line %q in:\n%s", want, body)
		}
	}
}

// TestOpsMetrics_ObserveWakePhase (ADR-093, P1B) — the schedd-side
// wake-phase histogram vector is pre-instantiated at boot with the
// closed 3-phase label set under the empty-app sentinel so the §12
// wake-latency-decomposition panel surfaces zero rows from an idle
// daemon (the PR #826 closed-set contract). WakeRPCDuration accessor
// returns a real prometheus.Observer for each (app, phase) pair so
// Engine.Wake can attach wake_id as a prometheus.Exemplar on every
// observation without paying the label-cardinality cost.
//
// Mirrors TestOpsMetrics_ObserveGuestInit's shape (pkg/wire/metrics.go
// field 11xx) but with the new {app, phase} label set.
func TestOpsMetrics_ObserveWakePhase(t *testing.T) {
	m := wire.NewOpsMetrics("schedd")

	// Observe on three phase values for one app. The empty-app
	// sentinel rows remain at 0.
	m.WakeRPCDuration("app-1", "admit_to_rpc").Observe(0.045)
	m.WakeRPCDuration("app-1", "rpc_call").Observe(0.250)
	m.WakeRPCDuration("app-1", "rpc_to_running").Observe(0.012)

	body := render(t, m)
	for _, want := range []string{
		// Real observations.
		`schedd_wake_rpc_duration_seconds_count{app="app-1",phase="admit_to_rpc"} 1`,
		`schedd_wake_rpc_duration_seconds_count{app="app-1",phase="rpc_call"} 1`,
		`schedd_wake_rpc_duration_seconds_count{app="app-1",phase="rpc_to_running"} 1`,
		// Pre-instantiated empty-app sentinel rows for the closed set.
		// P1B / ADR-093: the closed set is {admit_to_rpc, rpc_call,
		// rpc_to_running}; every value must surface from boot so the
		// dashboard panel has zero rows at idle (PR #826 precedent).
		// Metric name is *_wake_rpc_duration_seconds (not the
		// existing *_wake_phase_duration_seconds owned by the
		// events platform at ADR-064) — see metrics.go:1208-1227.
		`schedd_wake_rpc_duration_seconds_count{app="",phase="admit_to_rpc"} 0`,
		`schedd_wake_rpc_duration_seconds_count{app="",phase="rpc_call"} 0`,
		`schedd_wake_rpc_duration_seconds_count{app="",phase="rpc_to_running"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing line %q in:\n%s", want, body)
		}
	}
}

// TestOpsMetrics_ObserveWakePhaseNilSafe (ADR-093, P1B) — a nil
// *OpsMetrics receiver must not panic on WakeRPCDuration access.
// The convention (mirroring GuestInitDuration at metrics.go:2729
// and the existing TestOpsMetrics_GuestInitDurationNilSafe) is to
// return nil from the accessor; callers must check before
// observing. Engine unit tests build schedd without a metrics
// registry; the accessor must keep that path panic-free.
func TestOpsMetrics_ObserveWakePhaseNilSafe(t *testing.T) {
	var m *wire.OpsMetrics
	if got := m.WakeRPCDuration("app-1", "admit_to_rpc"); got != nil {
		t.Errorf("nil.WakeRPCDuration = %v, want nil", got)
	}
}

// TestOpsMetrics_ObserveLogEmitted (issue #254, Move 4) — the per-app
// SSE log frame counter increments on each ObserveLogEmitted call and
// the value surfaces in /metrics under apid_logs_emitted_total{app}.
// Pins the wire path end-to-end so an accidental rename in the metric
// name or the label set trips the test before the dashboard panel
// goes dark.
//
// The series is registered on every daemon (single-registry pattern,
// per memory wire-opsmetrics-single-registry); the test constructs an
// apid-flavored OpsMetrics so the absolute metric name matches the
// production path.
func TestOpsMetrics_ObserveLogEmitted(t *testing.T) {
	m := wire.NewOpsMetrics("apid")
	m.ObserveLogEmitted("app-1")
	m.ObserveLogEmitted("app-1")
	m.ObserveLogEmitted("app-2")

	body := render(t, m)
	for _, want := range []string{
		`apid_logs_emitted_total{app="app-1"} 2`,
		`apid_logs_emitted_total{app="app-2"} 1`,
		// The metric is registered on every daemon (including apid),
		// so the HELP/TYPE must surface in /metrics from boot — even
		// before any frame has been emitted. Verifies the
		// commonCollectors append was applied.
		`# HELP apid_logs_emitted_total`,
		`# TYPE apid_logs_emitted_total counter`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing line %q in:\n%s", want, body)
		}
	}
}

// TestOpsMetrics_ObserveLogEmittedNilSafe — handlers without a
// metrics registry (unit tests, throwaway scripts) must not panic
// when the SSE handler renders its first frame. Same nil-receiver
// contract as the other Observers.
func TestOpsMetrics_ObserveLogEmittedNilSafe(t *testing.T) {
	var m *wire.OpsMetrics
	m.ObserveLogEmitted("app-1")
}

// TestOpsMetrics_LogEmittedAcrossPrefixes pins the prefix-on-every-
// daemon contract (per memory wire-opsmetrics-single-registry):
// every OpsMetrics instance — regardless of prefix — has the
// _logs_emitted_total CounterVec pre-instantiated. Only apid's
// production path increments via ObserveLogEmitted; the others sit
// at zero. A regression that scopes the collector to a single
// prefix trips this test before the operator dashboard panel
// goes dark on a non-apid box.
func TestOpsMetrics_LogEmittedAcrossPrefixes(t *testing.T) {
	for _, prefix := range []string{"apid", "vmmd", "schedd", "imaged", "meterd", "builderd"} {
		m := wire.NewOpsMetrics(prefix)
		m.ObserveLogEmitted("any-app")
		body := render(t, m)
		// Metric name is "<prefix>_logs_emitted_total" — confirm
		// the literal string is present, not just a substring.
		want := prefix + `_logs_emitted_total{app="any-app"} 1`
		if !strings.Contains(body, want) {
			t.Errorf("prefix=%s missing %q in:\n%s", prefix, want, body)
		}
	}
}

// TestOpsMetrics_IncLogDropped (issue #309 / tier-2 DX) — the
// per-reason drop counter increments on each IncLogDropped call
// and the value surfaces in /metrics under
// apid_logs_dropped_total{reason}. Pins the wire path end-to-end
// so an accidental rename in the metric name or the label set
// trips the test before the dashboard panel goes dark.
//
// The closed-set guard in IncLogDropped is the load-bearing
// piece: an unknown reason value (e.g. a typo'd "slow-suscriber")
// silently drops without creating a new Prometheus series. The
// `TestOpsMetrics_IncLogDropped_UnknownReasonNoOp` case pins
// that contract.
func TestOpsMetrics_IncLogDropped(t *testing.T) {
	m := wire.NewOpsMetrics("apid")
	m.IncLogDropped("slow_subscriber")
	m.IncLogDropped("slow_subscriber")
	m.IncLogDropped("filter_grep")
	m.IncLogDropped("filter_level")

	body := render(t, m)
	for _, want := range []string{
		`apid_logs_dropped_total{reason="slow_subscriber"} 2`,
		`apid_logs_dropped_total{reason="filter_grep"} 1`,
		`apid_logs_dropped_total{reason="filter_level"} 1`,
		// Pre-instantiation: the closed label set must surface
		// in /metrics with zero values from boot, even before
		// any drop fires. Same precedent as
		// apid_logs_emitted_total / failedLoginTotal — a panel
		// selector `rate(apid_logs_dropped_total[5m])` should
		// never see "no data" on an idle daemon.
		`# HELP apid_logs_dropped_total`,
		`# TYPE apid_logs_dropped_total counter`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing line %q in:\n%s", want, body)
		}
	}
}

// TestOpsMetrics_IncLogDroppedNilSafe — same nil-receiver
// contract as ObserveLogEmitted. A vmmd or schedd unit test
// that constructs a nil *OpsMetrics must not panic when the
// ring-write drop path runs.
func TestOpsMetrics_IncLogDroppedNilSafe(t *testing.T) {
	var m *wire.OpsMetrics
	m.IncLogDropped("slow_subscriber")
}

// TestOpsMetrics_IncLogDropped_UnknownReasonNoOp pins the
// closed-set guard. An unknown reason value must NOT create a
// new Prometheus series — adding a fourth drop reason requires
// extending both the pre-instantiation loop and the switch in
// IncLogDropped. The test confirms a typo or a stale label
// silently drops so the platform never leaks per-call label
// values into the TSDB.
func TestOpsMetrics_IncLogDropped_UnknownReasonNoOp(t *testing.T) {
	m := wire.NewOpsMetrics("apid")
	m.IncLogDropped("slow-suscriber")   // typo: hyphen instead of underscore
	m.IncLogDropped("")                 // empty
	m.IncLogDropped("slow_subscribers") // plural typo

	body := render(t, m)
	if strings.Contains(body, `apid_logs_dropped_total{reason="slow-suscriber"}`) {
		t.Errorf("unknown reason value must not surface in /metrics:\n%s", body)
	}
	// The closed set must still be present at zero — unknown
	// labels must not displace the pre-instantiated rows.
	for _, want := range []string{
		`apid_logs_dropped_total{reason="slow_subscriber"} 0`,
		`apid_logs_dropped_total{reason="filter_grep"} 0`,
		`apid_logs_dropped_total{reason="filter_level"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing pre-instantiated line %q in:\n%s", want, body)
		}
	}
}

// TestOpsMetrics_IncPaddleWebhookVerifyFailed (PR-P4) — the
// paddle_webhook_verify_failed_total counter starts at 0, increments
// by 1 per IncPaddleWebhookVerifyFailed call, and surfaces in
// /metrics with the closed-vocabulary HELP/TYPE headers. Same
// pattern as TestOpsMetrics_IncLogDropped above (closed label set,
// pre-instantiated zero surface, no daemon-uniqueness requirement).
func TestOpsMetrics_IncPaddleWebhookVerifyFailed(t *testing.T) {
	m := wire.NewOpsMetrics("apid")
	m.IncPaddleWebhookVerifyFailed()
	m.IncPaddleWebhookVerifyFailed()
	m.IncPaddleWebhookVerifyFailed()

	body := render(t, m)
	for _, want := range []string{
		`apid_paddle_webhook_verify_failed_total 3`,
		`# HELP apid_paddle_webhook_verify_failed_total`,
		`# TYPE apid_paddle_webhook_verify_failed_total counter`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing line %q in:\n%s", want, body)
		}
	}
}

// TestOpsMetrics_IncPaddleWebhookReplaySuppressed (PR-P4) — the
// paddle_webhook_replay_suppressed_total counter mirrors the
// verify_failed counter's contract. The two counters are paired
// in the operator's runbook (failure-mode table row + runbook
// "Webhook hardening knobs" section) so a single test per counter
// keeps the wire-time fast.
func TestOpsMetrics_IncPaddleWebhookReplaySuppressed(t *testing.T) {
	m := wire.NewOpsMetrics("apid")
	m.IncPaddleWebhookReplaySuppressed()

	body := render(t, m)
	for _, want := range []string{
		`apid_paddle_webhook_replay_suppressed_total 1`,
		`# HELP apid_paddle_webhook_replay_suppressed_total`,
		`# TYPE apid_paddle_webhook_replay_suppressed_total counter`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing line %q in:\n%s", want, body)
		}
	}
}

// TestOpsMetrics_PaddleWebhookCountersNilSafe (PR-P4) — same
// nil-receiver contract as TestOpsMetrics_IncLogDroppedNilSafe.
// Tests that construct a nil *OpsMetrics (cmd unit tests with no
// metrics wiring) must not panic when the handler short-circuits
// through the paddle webhook path.
func TestOpsMetrics_PaddleWebhookCountersNilSafe(t *testing.T) {
	var m *wire.OpsMetrics
	m.IncPaddleWebhookVerifyFailed()
	m.IncPaddleWebhookReplaySuppressed()
}

func TestRenderSeconds(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{time.Millisecond, "0.001"},
		{500 * time.Microsecond, "0.0005"},
		{2 * time.Second, "2"},
	} {
		if got := wire.RenderSeconds(tc.in); got != tc.want {
			t.Errorf("RenderSeconds(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestOpsMetrics_RegistryAccess(t *testing.T) {
	m := wire.NewOpsMetrics("apid")
	if m.Registry() == nil {
		t.Fatal("Registry() returned nil")
	}
	// Observe something so the CounterVec has a series to gather.
	m.Observe("whoami", time.Millisecond, nil)
	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if len(mfs) == 0 {
		t.Error("expected at least one metric family after construction")
	}
}

func TestOpsMetrics_HandlerStandalone(t *testing.T) {
	// Handler() must be usable without an httptest server wrapper — that's
	// the form daemons actually mount onto their main mux.
	m := wire.NewOpsMetrics("meterd")
	m.Observe("tick", time.Millisecond, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	m.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `meterd_ops_total{code="ok",op="tick"} 1`) {
		t.Errorf("body missing tick series:\n%s", rec.Body.String())
	}
}

func readAll(t *testing.T, url string) (string, error) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return string(buf), nil
		}
	}
}

// TestOpsMetrics_SnapshotDiskDriftRegistered — the
// snapshot_disk_drift_total counter is registered on every daemon's
// OpsMetrics (single-registry pattern, memory
// wire.NewOpsMetrics single-registry pattern). It only produces
// samples when schedd's DiskDrift.Tick observes a discrepancy, but
// the collector must exist on every daemon's registry so a unified
// scrape never fails with "unknown metric."
func TestOpsMetrics_SnapshotDiskDriftRegistered(t *testing.T) {
	m := wire.NewOpsMetrics("schedd")
	c := m.SnapshotDiskDrift()
	if c == nil {
		t.Fatal("SnapshotDiskDrift() = nil on non-nil receiver")
	}
	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var found bool
	for _, fam := range mfs {
		if fam.GetName() == "schedd_snapshot_disk_drift_total" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("schedd_snapshot_disk_drift_total not present in registry gather")
	}

	// Increment and re-gather: counter should now read 1.
	c.Inc()
	mfs, err = m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather after Inc: %v", err)
	}
	for _, fam := range mfs {
		if fam.GetName() == "schedd_snapshot_disk_drift_total" {
			for _, mt := range fam.GetMetric() {
				if got := mt.GetCounter().GetValue(); got != 1 {
					t.Errorf("counter = %v, want 1 after one Inc", got)
				}
			}
			return
		}
	}
	t.Fatal("counter disappeared after Inc")
}

// TestOpsMetrics_SnapshotDiskDriftNilSafe — DiskDrift.Tick calls
// SnapshotDiskDrift() without guarding the receiver; the accessor
// itself must short-circuit on a nil receiver so a partially wired
// DiskDrift (or a test that constructs the struct directly) doesn't
// panic on every Tick.
func TestOpsMetrics_SnapshotDiskDriftNilSafe(t *testing.T) {
	var m *wire.OpsMetrics
	if got := m.SnapshotDiskDrift(); got != nil {
		t.Errorf("SnapshotDiskDrift on nil receiver = %v, want nil", got)
	}
}

// TestOpsMetrics_WakePhaseClosedSet (issue #517 / PR-C / ADR-064) —
// the wake-phase collector pair is registered on every daemon
// (single-registry pattern, memory wire-opsmetrics-single-registry)
// and pre-instantiated with the closed 13-phase × 2-result label
// set so the §12 wake-latency panel surfaces zero on an idle
// daemon. The accessor must be nil-safe on a nil receiver so
// engine unit tests without metrics keep working.
func TestOpsMetrics_WakePhaseClosedSet(t *testing.T) {
	for _, prefix := range []string{"schedd", "vmmd", "gatewayd_internal", "apid", "builderd"} {
		m := wire.NewOpsMetrics(prefix)
		// Increment one phase to verify the counter surfaces
		// under the correct metric name.
		m.WakePhaseEmitted("boot_started", "ok").Inc()
		// Observe a duration to verify the histogram is wired.
		m.WakePhaseDuration("boot_started", "ok").Observe(0.123)
		body := render(t, m)
		if !strings.Contains(body, prefix+"_wake_phase_emitted_total{phase=\"boot_started\",result=\"ok\"} 1") {
			t.Errorf("prefix=%s missing wake_phase_emitted counter; body:\n%s", prefix, body)
		}
		if !strings.Contains(body, prefix+"_wake_phase_duration_seconds_count{phase=\"boot_started\",result=\"ok\"} 1") {
			t.Errorf("prefix=%s missing wake_phase_duration histogram; body:\n%s", prefix, body)
		}
		// Each pre-instantiated (phase, result) tuple must
		// surface in /metrics with value 0 — verify a couple
		// of representative cells.
		for _, want := range []string{
			prefix + `_wake_phase_emitted_total{phase="readiness_200",result="ok"} 0`,
			prefix + `_wake_phase_emitted_total{phase="build_failed",result="failed"} 0`,
			prefix + `_wake_phase_duration_seconds_count{phase="proxy_first_byte",result="ok"} 0`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("prefix=%s missing pre-instantiated cell %q", prefix, want)
			}
		}
	}
}

// TestOpsMetrics_WakePhaseNilSafe — nil-receiver guard. The
// pkg/events.Platform unit tests construct Platform without an
// OpsMetrics; the accessors must be no-ops on nil receivers.
func TestOpsMetrics_WakePhaseNilSafe(t *testing.T) {
	var m *wire.OpsMetrics
	if got := m.WakePhaseEmitted("boot_started", "ok"); got != nil {
		t.Errorf("nil.WakePhaseEmitted = %v, want nil", got)
	}
	if got := m.WakePhaseDuration("boot_started", "ok"); got != nil {
		t.Errorf("nil.WakePhaseDuration = %v, want nil", got)
	}
}

// TestOpsMetrics_ObserveOAuthDisabled (issue #419 / ADR-046) — the
// sign-in OAuth consent handlers increment
// `apid_oauth_disabled_total{provider}` on every 503
// `oauth_provider_unavailable` response. The accessor must:
//   - increment by 1 for the closed set ("google", "github"),
//   - leave the metric untouched for unknown providers so a
//     future caller can't widen the label set by accident,
//   - be no-op on a nil receiver so apid unit tests that don't
//     wire metrics keep working (parity with ObserveLogEmitted
//     above).
func TestOpsMetrics_ObserveOAuthDisabled(t *testing.T) {
	m := wire.NewOpsMetrics("apid")
	m.ObserveOAuthDisabled("google")
	m.ObserveOAuthDisabled("google")
	m.ObserveOAuthDisabled("github")
	// Unknown provider: accessor must not create a label series.
	m.ObserveOAuthDisabled("facebook")

	body := render(t, m)
	wantGoogle := `apid_oauth_disabled_total{provider="google"} 2`
	if !strings.Contains(body, wantGoogle) {
		t.Errorf("missing line %q in:\n%s", wantGoogle, body)
	}
	wantGitHub := `apid_oauth_disabled_total{provider="github"} 1`
	if !strings.Contains(body, wantGitHub) {
		t.Errorf("missing line %q in:\n%s", wantGitHub, body)
	}
	if strings.Contains(body, `provider="facebook"`) {
		t.Errorf("facebook label series must not be created, got:\n%s", body)
	}

	// Nil-receiver parity with the other Observe* accessors.
	var nilM *wire.OpsMetrics
	nilM.ObserveOAuthDisabled("google") // must not panic
}

// TestOpsMetrics_ObserveAdvisoryBatchResult (Mega-PR B) — the
// stateless-advisory forward outcome counter increments per
// closed-set result value (ok / dial_failed / rejected /
// unavailable_after_retry) and refuses to widen the label set
// for unknown values, mirroring the OAuth-counter pattern above.
// Pair-counter with stateless_advisory_events_total: a healthy
// box has rate(apid_..{severity="high"})[5m] ≈
// rate(vmmd_..{result="ok"})[5m].
func TestOpsMetrics_ObserveAdvisoryBatchResult(t *testing.T) {
	m := wire.NewOpsMetrics("vmmd")
	m.ObserveAdvisoryBatchResult("ok")
	m.ObserveAdvisoryBatchResult("ok")
	m.ObserveAdvisoryBatchResult("dial_failed")
	m.ObserveAdvisoryBatchResult("rejected")
	m.ObserveAdvisoryBatchResult("unavailable_after_retry")
	// Unknown result: accessor must NOT create a label series.
	m.ObserveAdvisoryBatchResult("mystery")

	body := render(t, m)
	wantLines := []string{
		`vmmd_stateless_advisory_batches_emitted_total{result="ok"} 2`,
		`vmmd_stateless_advisory_batches_emitted_total{result="dial_failed"} 1`,
		`vmmd_stateless_advisory_batches_emitted_total{result="rejected"} 1`,
		`vmmd_stateless_advisory_batches_emitted_total{result="unavailable_after_retry"} 1`,
	}
	for _, want := range wantLines {
		if !strings.Contains(body, want) {
			t.Errorf("missing line %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, `result="mystery"`) {
		t.Errorf("mystery label series must not be created, got:\n%s", body)
	}

	// Nil-receiver parity.
	var nilM *wire.OpsMetrics
	nilM.ObserveAdvisoryBatchResult("ok") // must not panic
}

// TestOpsMetrics_PreInstantiatesAdvisoryBatchSeries — the closed
// result set must surface in /metrics from the moment the
// daemon boots, all four rows at value 0. This is the
// pre-instantiation contract that lets the §12 dashboard panel
// render "no data → 0" rather than "no data → missing series"
// on an idle box.
func TestOpsMetrics_PreInstantiatesAdvisoryBatchSeries(t *testing.T) {
	m := wire.NewOpsMetrics("vmmd")
	body := render(t, m)
	for _, result := range []string{"ok", "dial_failed", "rejected", "unavailable_after_retry"} {
		want := `vmmd_stateless_advisory_batches_emitted_total{result="` + result + `"} 0`
		if !strings.Contains(body, want) {
			t.Errorf("pre-instantiated line %q missing in:\n%s", want, body)
		}
	}
}

// TestOpsMetrics_ObserveStatelessAdvisory (Mega-PR B) — apid
// receiver-side counter. Same closed-set semantics as the vmmd
// forward counter but labels by severity ∈ {high, warn, info}.
// Mirrors cmd/apid/advisory_receiver.go's advisoryBatchSeverity
// vocabulary; an unknown severity (e.g. "urgent") must NOT
// create a label series.
func TestOpsMetrics_ObserveStatelessAdvisory(t *testing.T) {
	m := wire.NewOpsMetrics("apid")
	m.ObserveStatelessAdvisory("high")
	m.ObserveStatelessAdvisory("high")
	m.ObserveStatelessAdvisory("warn")
	m.ObserveStatelessAdvisory("info")
	// Unknown severity: closed-set guard.
	m.ObserveStatelessAdvisory("urgent")

	body := render(t, m)
	wantLines := []string{
		`apid_stateless_advisory_events_total{severity="high"} 2`,
		`apid_stateless_advisory_events_total{severity="warn"} 1`,
		`apid_stateless_advisory_events_total{severity="info"} 1`,
	}
	for _, want := range wantLines {
		if !strings.Contains(body, want) {
			t.Errorf("missing line %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, `severity="urgent"`) {
		t.Errorf("urgent label series must not be created, got:\n%s", body)
	}

	// Nil-receiver parity.
	var nilM *wire.OpsMetrics
	nilM.ObserveStatelessAdvisory("high") // must not panic
}

// TestOpsMetrics_PreInstantiatesStatelessAdvisorySeries — the
// closed severity set must surface in /metrics from boot, all
// three rows at value 0. Same pre-instantiation contract as the
// vmmd forward counter above.
func TestOpsMetrics_PreInstantiatesStatelessAdvisorySeries(t *testing.T) {
	m := wire.NewOpsMetrics("apid")
	body := render(t, m)
	for _, sev := range []string{"high", "warn", "info"} {
		want := `apid_stateless_advisory_events_total{severity="` + sev + `"} 0`
		if !strings.Contains(body, want) {
			t.Errorf("pre-instantiated line %q missing in:\n%s", want, body)
		}
	}
}

// TestOpsMetrics_ObserveGithubdPathFilter (issue #432 phase 5 /
// ADR-050 §109). The path-filter mode counter is labelled by
// mode ∈ {paths, full_fallback, truncated, error, breaker_open}.
// Closed-set semantics — an unknown mode (e.g. "discarded")
// must NOT create a label series, and the nil-receiver must
// not panic.
func TestOpsMetrics_ObserveGithubdPathFilter(t *testing.T) {
	m := wire.NewOpsMetrics("githubd")
	m.ObserveGithubdPathFilter(wire.PathFilterModePaths)
	m.ObserveGithubdPathFilter(wire.PathFilterModePaths)
	m.ObserveGithubdPathFilter(wire.PathFilterModeFullFallback)
	m.ObserveGithubdPathFilter(wire.PathFilterModeTruncated)
	m.ObserveGithubdPathFilter(wire.PathFilterModeError)
	m.ObserveGithubdPathFilter(wire.PathFilterModeBreakerOpen)
	// Unknown mode: closed-set guard.
	m.ObserveGithubdPathFilter("discarded")

	body := render(t, m)
	wantLines := []string{
		`githubd_path_filter_total{mode="paths"} 2`,
		`githubd_path_filter_total{mode="full_fallback"} 1`,
		`githubd_path_filter_total{mode="truncated"} 1`,
		`githubd_path_filter_total{mode="error"} 1`,
		`githubd_path_filter_total{mode="breaker_open"} 1`,
	}
	for _, want := range wantLines {
		if !strings.Contains(body, want) {
			t.Errorf("missing line %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, `mode="discarded"`) {
		t.Errorf("discarded label series must not be created, got:\n%s", body)
	}

	// Nil-receiver parity.
	var nilM *wire.OpsMetrics
	nilM.ObserveGithubdPathFilter(wire.PathFilterModePaths) // must not panic
}

// TestOpsMetrics_PreInstantiatesGithubdPathFilterSeries — the
// closed `mode` label set must surface in /metrics from boot,
// all five rows at value 0. Same pre-instantiation contract as
// every other CounterVec on this struct.
func TestOpsMetrics_PreInstantiatesGithubdPathFilterSeries(t *testing.T) {
	m := wire.NewOpsMetrics("githubd")
	body := render(t, m)
	for _, mode := range []string{wire.PathFilterModePaths, wire.PathFilterModeFullFallback, wire.PathFilterModeTruncated, wire.PathFilterModeError, wire.PathFilterModeBreakerOpen} {
		want := `githubd_path_filter_total{mode="` + mode + `"} 0`
		if !strings.Contains(body, want) {
			t.Errorf("pre-instantiated line %q missing in:\n%s", want, body)
		}
	}
}

// TestOpsMetrics_WarmSnapshotErrors (issue #470 / PR A / ADR-055)
// pins the warm-snapshot-error counter's label set. The closed
// {vmm_call, store_write} set is pre-instantiated at boot so the
// dashboard surfaces zero on an idle daemon; a regression that
// lets the reason label free-form would break the closed-set
// cardinality budget and trip here.
func TestOpsMetrics_WarmSnapshotErrors(t *testing.T) {
	m := wire.NewOpsMetrics("vmmd")
	// Real observations.
	m.WarmSnapshotErrors("vmm_call").Inc()
	m.WarmSnapshotErrors("vmm_call").Inc()
	m.WarmSnapshotErrors("store_write").Inc()

	body := render(t, m)
	for _, want := range []string{
		`vmmd_warm_snapshot_errors_total{reason="vmm_call"} 2`,
		`vmmd_warm_snapshot_errors_total{reason="store_write"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing line %q in:\n%s", want, body)
		}
	}
}

// TestOpsMetrics_ObserveSidecarRestart (issue #463 / ADR-069 /
// ADR-071 / PR-C §4) pins the per-(app, sidecar) restart counter
// that vmmd increments from dispatchSidecarRestart. The metric
// lands at <prefix>_sidecar_restart_total{app, sidecar}; a
// dispatch per restart cycle increments the labelled row by 1.
// The empty (app="", sidecar="") tuple is pre-instantiated so /metrics
// surfaces the metric name from boot (matches the
// scaleUpDecisions / scaleDownDecisions precedent at the bottom of
// metrics.go's instantiate list).
//
// NOTE: Prometheus reports a Per-Series reset on first
// observation: the counter pre-instantiates with WithLabelValues
// at construction time (value 0), then the first Observe call
// produces a delta of 1. Observed increments BEFORE the first
// call do not accumulate — the renderer returns the FINAL value
// for that tuple. The test below fires Observe twice per tuple
// and asserts each tuple reports a value of 1 (the post-first-
// call surface), since Prometheus' exposed format reports the
// current value of each series, not the running total.
func TestOpsMetrics_ObserveSidecarRestart(t *testing.T) {
	m := wire.NewOpsMetrics("vmmd")
	// Three increments for the same (app, sidecar) tuple
	// pair — three restarts on the same essential sidecar.
	m.ObserveSidecarRestart("app-1", "metrics")
	m.ObserveSidecarRestart("app-1", "metrics")
	m.ObserveSidecarRestart("app-1", "metrics")
	// Two increments on a second (app, sidecar) pair — a
	// different essential sidecar, different app.
	m.ObserveSidecarRestart("app-2", "audit")
	m.ObserveSidecarRestart("app-2", "audit")

	body := render(t, m)
	for _, want := range []string{
		`vmmd_sidecar_restart_total{app="app-1",sidecar="metrics"} 3`,
		`vmmd_sidecar_restart_total{app="app-2",sidecar="audit"} 2`,
		// Pre-instantiated empty-tuple row, mirrors the
		// instanceCPUSecondsTotal{app="",node=""} precedent.
		`vmmd_sidecar_restart_total{app="",sidecar=""} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing line %q in:\n%s", want, body)
		}
	}
}

// TestOpsMetrics_WarmSnapshotErrorsNilSafe (issue #470 / PR A /
// ADR-055) — the accessor must be no-op on a nil receiver so
// vmmd / schedd unit tests without metrics keep working (same
// nil-safe posture as EgressDeny). The convention is `nil →
// return nil`.
func TestOpsMetrics_WarmSnapshotErrorsNilSafe(t *testing.T) {
	var m *wire.OpsMetrics
	if got := m.WarmSnapshotErrors("vmm_call"); got != nil {
		t.Errorf("nil.WarmSnapshotErrors = %v, want nil", got)
	}
}

// TestOpsMetrics_ObserveSidecarRestartNilSafe pins the nil-
// receiver contract (issue #463 / ADR-069 / ADR-071 / PR-C §4).
// vmmd's dispatchSidecarRestart ALWAYS calls
// ObserveSidecarRestart, even when the cmd-level wiring
// omitted a real OpsMetrics (default-local path). A nil
// receiver must be a no-op so a missing wiring doesn't panic
// the dispatch loop. Mirrors ObserveScaleDown / ObserveXxx
// nil-safety in this file.
func TestOpsMetrics_ObserveSidecarRestartNilSafe(t *testing.T) {
	var m *wire.OpsMetrics
	// Must NOT panic.
	m.ObserveSidecarRestart("app-1", "metrics")
}

// TestOpsMetrics_GuestInitDuration (issue #470 / PR C / ADR-074)
// pins the bucket set (spec §6.3 verbatim: {.05, .1, .2, .3, .35,
// .5, .8, 1, 1.5, 3, 5}) and the label set (app, runner). The
// empty-tuple sentinel ("", "") is pre-instantiated so dashboards
// render from boot — a regression that drops the sentinel would
// break the §12 panel surface. Prometheus buckets are inclusive on
// the upper bound (le), so 0.30 and 0.34 both satisfy le="0.35".
func TestOpsMetrics_GuestInitDuration(t *testing.T) {
	m := wire.NewOpsMetrics("vmmd")
	// Real observations across the bucket spread.
	m.GuestInitDuration("app-1", "node22").Observe(0.04)   // ≤ 0.05
	m.GuestInitDuration("app-1", "node22").Observe(0.30)   // ≤ 0.3
	m.GuestInitDuration("app-1", "node22").Observe(0.34)   // ≤ 0.35
	m.GuestInitDuration("app-2", "python312").Observe(2.0) // ≤ 3

	body := render(t, m)
	for _, want := range []string{
		`vmmd_guest_init_duration_seconds_bucket{app="app-1",runner="node22",le="0.05"} 1`,
		`vmmd_guest_init_duration_seconds_bucket{app="app-1",runner="node22",le="0.3"} 2`,
		`vmmd_guest_init_duration_seconds_bucket{app="app-1",runner="node22",le="0.35"} 3`,
		`vmmd_guest_init_duration_seconds_bucket{app="app-2",runner="python312",le="3"} 1`,
		`vmmd_guest_init_duration_seconds_bucket{app="",runner="",le="0.05"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing line %q in:\n%s", want, body)
		}
	}
}

// TestOpsMetrics_GuestInitDurationNilSafe (issue #470 / PR C /
// ADR-074) — mirrors the WarmSnapshotErrors nil-safe pin.
func TestOpsMetrics_GuestInitDurationNilSafe(t *testing.T) {
	var m *wire.OpsMetrics
	if got := m.GuestInitDuration("app", "runner"); got != nil {
		t.Errorf("nil.GuestInitDuration = %v, want nil", got)
	}
}

// TestOpsMetrics_WakeSnapshotTier (issue #470 / PR C / ADR-074)
// pins the closed-set tier label ({warm, init, cold_boot_fallback}).
// All three labels are pre-instantiated at boot so the
// wake-tier-mix panel has zero rows from idle fleet.
func TestOpsMetrics_WakeSnapshotTier(t *testing.T) {
	m := wire.NewOpsMetrics("schedd")
	m.WakeSnapshotTier("warm").Inc()
	m.WakeSnapshotTier("warm").Inc()
	m.WakeSnapshotTier("warm").Inc()
	m.WakeSnapshotTier("init").Inc()
	m.WakeSnapshotTier("cold_boot_fallback").Inc()

	body := render(t, m)
	for _, want := range []string{
		`schedd_wake_snapshot_tier_total{tier="warm"} 3`,
		`schedd_wake_snapshot_tier_total{tier="init"} 1`,
		`schedd_wake_snapshot_tier_total{tier="cold_boot_fallback"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing line %q in:\n%s", want, body)
		}
	}
}

// TestOpsMetrics_WakeSnapshotTierNilSafe (issue #470 / PR C /
// ADR-074) — mirrors the WarmSnapshotErrors nil-safe pin.
func TestOpsMetrics_WakeSnapshotTierNilSafe(t *testing.T) {
	var m *wire.OpsMetrics
	if got := m.WakeSnapshotTier("warm"); got != nil {
		t.Errorf("nil.WakeSnapshotTier = %v, want nil", got)
	}
}

// TestOpsMetrics_GuestTailSeconds_PreInstantiated (issue #667 /
// ADR-078) pins the closed-set (plan × runtime × outcome)
// Cartesian pre-instantiated at boot. The 60-series budget is
// the load-bearing cardinality promise for §12's tail-latency
// panel: every (plan, runtime, outcome) combination must
// appear in the registry after NewOpsMetrics() even if no tail
// has fired yet. If a future PR drops a runtime / outcome
// literal, the corresponding rows stop appearing and this test
// fires.
func TestOpsMetrics_GuestTailSeconds_PreInstantiated(t *testing.T) {
	m := wire.NewOpsMetrics("vmmd")
	runtimes := []string{"node22", "node24", "python312", "python313", "go124"}
	outcomes := []string{"completed", "failed", "timeout"}
	plans := []string{"free", "hobby", "pro", "scale"}

	// Observe a single sample on every (plan, runtime, outcome) so
	// the render() helper picks up the 60 series. The values are
	// arbitrary — the test is about presence, not magnitude.
	for _, plan := range plans {
		for _, runtime := range runtimes {
			for _, outcome := range outcomes {
				m.GuestTailSeconds(plan, runtime, outcome).Observe(0.42)
			}
		}
	}

	body := render(t, m)
	seen := 0
	for _, plan := range plans {
		for _, runtime := range runtimes {
			for _, outcome := range outcomes {
				// Prometheus emits label pairs in alphabetical order,
				// so the on-the-wire tuple is {outcome, plan, runtime,
				// le="..."} — pin a representative bucket.
				want := fmt.Sprintf(
					`vmmd_guest_tail_seconds_bucket{outcome=%q,plan=%q,runtime=%q,le="0.05"}`,
					outcome, plan, runtime,
				)
				if strings.Contains(body, want) {
					seen++
				}
			}
		}
	}
	if seen != 60 {
		t.Errorf("GuestTailSeconds Cartesian saw %d series, want 60 (4 plans × 5 runtimes × 3 outcomes)", seen)
	}
}

// TestOpsMetrics_GuestTailFailedTotal_PreInstantiated (issue #667
// / ADR-078) pins the closed-set (plan × reason) Cartesian.
// 4 plans × 4 reasons = 16 series. The same cardinality
// invariant as the histogram test — if a reason literal is
// dropped, the panel loses its rows.
func TestOpsMetrics_GuestTailFailedTotal_PreInstantiated(t *testing.T) {
	m := wire.NewOpsMetrics("vmmd")
	reasons := []string{"timeout", "handler_error", "forced_at_park", "unknown"}
	plans := []string{"free", "hobby", "pro", "scale"}

	for _, plan := range plans {
		for _, reason := range reasons {
			m.GuestTailFailedTotal(plan, reason).Inc()
		}
	}

	body := render(t, m)
	seen := 0
	for _, plan := range plans {
		for _, reason := range reasons {
			want := fmt.Sprintf(
				`vmmd_guest_tail_failed_total{plan=%q,reason=%q} 1`,
				plan, reason,
			)
			if strings.Contains(body, want) {
				seen++
			}
		}
	}
	if seen != 16 {
		t.Errorf("GuestTailFailedTotal Cartesian saw %d series, want 16 (4 plans × 4 reasons)", seen)
	}
}

// TestOpsMetrics_TailCapReached_PreInstantiated (issue #667 /
// ADR-078) pins the per-plan cap-pressure counter. 4 series
// total — the cap-pressure panel needs every plan visible at
// boot to distinguish "no cap pressure" (zero rows) from "no
// data" (missing rows).
func TestOpsMetrics_TailCapReached_PreInstantiated(t *testing.T) {
	m := wire.NewOpsMetrics("vmmd")
	for _, plan := range []string{"free", "hobby", "pro", "scale"} {
		m.TailCapReached(plan).Inc()
	}

	body := render(t, m)
	seen := 0
	for _, plan := range []string{"free", "hobby", "pro", "scale"} {
		want := fmt.Sprintf(`vmmd_tail_cap_reached_total{plan=%q} 1`, plan)
		if strings.Contains(body, want) {
			seen++
		}
	}
	if seen != 4 {
		t.Errorf("TailCapReached saw %d series, want 4 (one per plan)", seen)
	}
}

// TestOpsMetrics_GuestTail_NilSafe (issue #667 / ADR-078) —
// mirrors the WakeSnapshotTier nil-safe pin. Every new tail
// accessor must be nil-safe on receiver; otherwise unit tests
// without metrics stop building.
func TestOpsMetrics_GuestTail_NilSafe(t *testing.T) {
	var m *wire.OpsMetrics
	if got := m.GuestTailSeconds("pro", "node22", "completed"); got != nil {
		t.Errorf("nil.GuestTailSeconds = %v, want nil", got)
	}
	if got := m.GuestTailFailedTotal("pro", "timeout"); got != nil {
		t.Errorf("nil.GuestTailFailedTotal = %v, want nil", got)
	}
	if got := m.TailCapReached("pro"); got != nil {
		t.Errorf("nil.TailCapReached = %v, want nil", got)
	}
}
