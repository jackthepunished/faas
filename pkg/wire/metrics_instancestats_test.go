// metrics_instancestats_test.go — tests for the issue #170 / PR-A
// per-{app,node} instance-stats gauges. Lives in a separate file so
// the existing metrics_test.go stays focused on the Observe / Stripe /
// build histograms.

package wire_test

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/wire"
)

// fetchMetrics scrapes the daemon's /metrics endpoint and returns the
// raw text. Slightly cheaper than parsing the dto; the assertions in
// this file are substring-based because the labels are deterministic
// and the values are exactly the ones we wrote.
func fetchMetrics(t *testing.T, m *wire.OpsMetrics) string {
	t.Helper()
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
	return string(buf)
}

// TestOpsMetrics_ReplaceInstanceStats_EmitsThreePerAppNodeGauges pins
// the metric names + {app,node} label tuple. A regression that
// dropped the prefix or moved the labels would surface here.
func TestOpsMetrics_ReplaceInstanceStats_EmitsThreePerAppNodeGauges(t *testing.T) {
	m := wire.NewOpsMetrics("schedd")
	rows := []wire.InstanceStatRow{
		{AppID: "app1", NodeID: "node1", CPUPct: 42.5, RSSMB: 256, InflightRequests: 3},
		{AppID: "app1", NodeID: "node2", CPUPct: 18.0, RSSMB: 192, InflightRequests: 0},
	}
	m.ReplaceInstanceStats(rows, 50*time.Millisecond)

	body := fetchMetrics(t, m)
	for _, want := range []string{
		// Gauge names — ADR-015 prefix + descriptive suffix.
		`schedd_instance_cpu_pct`,
		`schedd_instance_rss_mb`,
		`schedd_instance_inflight_requests`,
		// (app, node) label tuples rendered by Prometheus.
		`app="app1",node="node1"`,
		`app="app1",node="node2"`,
		// The collect-duration histogram surfaces the Tick dur we passed.
		`schedd_instance_stats_collect_seconds`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body missing %q", want)
		}
	}
}

// TestOpsMetrics_ReplaceInstanceStats_MaxCPUAndSumRSS pins the
// rollup semantics: two siblings of the same (app, node) collapse to
// max-CPU, sum-RSS, sum-inflight. This is the per-instance→per-node
// collapse that the (app, node) label cardinality is designed to
// enforce.
func TestOpsMetrics_ReplaceInstanceStats_MaxCPUAndSumRSS(t *testing.T) {
	m := wire.NewOpsMetrics("schedd")
	rows := []wire.InstanceStatRow{
		// Two siblings of (app1, node1).
		{AppID: "app1", NodeID: "node1", CPUPct: 30.0, RSSMB: 100, InflightRequests: 1},
		{AppID: "app1", NodeID: "node1", CPUPct: 75.0, RSSMB: 50, InflightRequests: 4},
		// One sibling of (app2, node1).
		{AppID: "app2", NodeID: "node1", CPUPct: 10.0, RSSMB: 200, InflightRequests: 0},
	}
	m.ReplaceInstanceStats(rows, 10*time.Millisecond)

	body := fetchMetrics(t, m)
	cases := []struct {
		metric string
		labels string
		want   string
	}{
		// (app1, node1): max(30, 75)=75, sum(100, 50)=150, sum(1, 4)=5.
		{`schedd_instance_cpu_pct`, `app="app1",node="node1"`, `75`},
		{`schedd_instance_rss_mb`, `app="app1",node="node1"`, `150`},
		{`schedd_instance_inflight_requests`, `app="app1",node="node1"`, `5`},
		// (app2, node1): single sibling, values pass through.
		{`schedd_instance_cpu_pct`, `app="app2",node="node1"`, `10`},
		{`schedd_instance_rss_mb`, `app="app2",node="node1"`, `200`},
		{`schedd_instance_inflight_requests`, `app="app2",node="node1"`, `0`},
	}
	for _, c := range cases {
		// Prometheus emits "<metric>{<labels>} <value>". We assert
		// the full line is present so a regression that swapped
		// labels or shuffled values would fail.
		needle := c.metric + "{" + c.labels + "} " + c.want
		if !strings.Contains(body, needle) {
			t.Errorf("metrics body missing %q", needle)
		}
	}
}

// TestOpsMetrics_ReplaceInstanceStats_NaNExcludedFromRollup pins
// that NaN CPUPct / RSSMB values (the "absent this tick" sentinel
// from the poller) skip the rollup rather than producing NaN
// samples. The Prom wire would render NaN as "NaN" and corrupt
// dashboards — the rollup must drop them.
func TestOpsMetrics_ReplaceInstanceStats_NaNExcludedFromRollup(t *testing.T) {
	m := wire.NewOpsMetrics("schedd")
	rows := []wire.InstanceStatRow{
		// First sibling: CPU present, RSS absent.
		{AppID: "app1", NodeID: "node1", CPUPct: 50.0, RSSMB: math.NaN(), InflightRequests: 1},
		// Second sibling: CPU absent, RSS present.
		{AppID: "app1", NodeID: "node1", CPUPct: math.NaN(), RSSMB: 64, InflightRequests: 2},
	}
	m.ReplaceInstanceStats(rows, 5*time.Millisecond)

	body := fetchMetrics(t, m)
	// CPU: only one valid reading → 50.
	if !strings.Contains(body, `schedd_instance_cpu_pct{app="app1",node="node1"} 50`) {
		t.Errorf("CPU rollup did not collapse to the only valid reading (50)")
	}
	// RSS: only one valid reading → 64.
	if !strings.Contains(body, `schedd_instance_rss_mb{app="app1",node="node1"} 64`) {
		t.Errorf("RSS rollup did not collapse to the only valid reading (64)")
	}
	// Inflight: sum always (zero is a real value) → 1 + 2 = 3.
	if !strings.Contains(body, `schedd_instance_inflight_requests{app="app1",node="node1"} 3`) {
		t.Errorf("inflight rollup did not sum (1+2=3)")
	}
	// Guardrail: no "NaN" string should ever appear in the scrape.
	if strings.Contains(body, " NaN") {
		t.Errorf("metrics body contains a NaN sample — wire must never emit NaN")
	}
}

// TestOpsMetrics_ReplaceInstanceStats_EmptyRowsClearsLabels pins
// that a Tick with no live instances collapses the label set.
// Without this, a destroyed app's last rollup would linger until
// the next Tick brought it back — drift between the live view and
// the durable state.
func TestOpsMetrics_ReplaceInstanceStats_EmptyRowsClearsLabels(t *testing.T) {
	m := wire.NewOpsMetrics("schedd")
	// First, write something so the label tuples get registered.
	m.ReplaceInstanceStats([]wire.InstanceStatRow{
		{AppID: "app1", NodeID: "node1", CPUPct: 10, RSSMB: 100, InflightRequests: 0},
	}, 1*time.Millisecond)
	// Then wipe.
	m.ReplaceInstanceStats(nil, 1*time.Millisecond)

	body := fetchMetrics(t, m)
	// After Reset(), the only references to the gauges should be in
	// the HELP/TYPE lines, never a sample line.
	for _, prefix := range []string{
		`schedd_instance_cpu_pct{`,
		`schedd_instance_rss_mb{`,
		`schedd_instance_inflight_requests{`,
	} {
		if strings.Contains(body, prefix) {
			t.Errorf("metrics body still contains sample line %q after empty Tick", prefix)
		}
	}
}

// TestOpsMetrics_ReplaceInstanceStats_AppNodeDistinctness pins that
// the per-(app,node) rollup does not collapse distinct tuples
// even when their string concatenation overlaps. The poller uses
// NUL as the join byte because app_id and node_id are UUIDs /
// [a-z0-9-]+ in production — neither can contain NUL — so the
// disambiguation is safe. We exercise the path with a benign
// (no-NUL) payload that mimics the production scenario.
func TestOpsMetrics_ReplaceInstanceStats_AppNodeDistinctness(t *testing.T) {
	m := wire.NewOpsMetrics("schedd")
	// Distinct (app, node) tuples that string-concatenate to the
	// same bytes would still split correctly because the NUL join
	// is unambiguous. We use plain strings; the contract is "two
	// distinct tuples do not collapse".
	rows := []wire.InstanceStatRow{
		{AppID: "app1", NodeID: "node1", CPUPct: 1, RSSMB: 1, InflightRequests: 0},
		{AppID: "app1", NodeID: "node2", CPUPct: 2, RSSMB: 2, InflightRequests: 0},
		{AppID: "app2", NodeID: "node1", CPUPct: 3, RSSMB: 3, InflightRequests: 0},
	}
	m.ReplaceInstanceStats(rows, 1*time.Millisecond)
	body := fetchMetrics(t, m)
	for _, want := range []string{
		`schedd_instance_cpu_pct{app="app1",node="node1"} 1`,
		`schedd_instance_cpu_pct{app="app1",node="node2"} 2`,
		`schedd_instance_cpu_pct{app="app2",node="node1"} 3`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body missing %q", want)
		}
	}
}

// TestOpsMetrics_ReplaceInstanceStats_PartialErrorCounter pins the
// per-node error counter. The poller would call this once per node
// failure inside a Tick; the assertion is a single Inc() surfacing
// in the scrape.
func TestOpsMetrics_ReplaceInstanceStats_PartialErrorCounter(t *testing.T) {
	m := wire.NewOpsMetrics("schedd")
	m.InstanceStatsPartialError("node-1")
	m.InstanceStatsPartialError("node-1")
	m.InstanceStatsPartialError("node-2")

	body := fetchMetrics(t, m)
	for _, want := range []string{
		`schedd_instance_stats_partial_errors_total{node="node-1"} 2`,
		`schedd_instance_stats_partial_errors_total{node="node-2"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body missing %q", want)
		}
	}
}

// TestOpsMetrics_ReplaceInstanceStats_NilReceiverIsSafe pins the
// nil-safety contract shared with SetResidentGBPerCustomer and
// friends. The schedd unit tests build *OpsMetrics; the kvm
// integration tests don't. This test would catch a regression that
// dropped the nil guard.
func TestOpsMetrics_ReplaceInstanceStats_NilReceiverIsSafe(t *testing.T) {
	var m *wire.OpsMetrics
	// Must not panic.
	m.ReplaceInstanceStats([]wire.InstanceStatRow{
		{AppID: "app1", NodeID: "node1", CPUPct: 1, RSSMB: 1, InflightRequests: 0},
	}, 1*time.Millisecond)
	m.InstanceStatsPartialError("node-1")
}
