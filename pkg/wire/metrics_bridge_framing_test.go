// Tests for the bridgeFramingTotal counter (ADR-127 §D3, Layer 7).
// Pins:
//   - The 12-series cross-product is pre-instantiated at boot
//     (so the bridge-protection Grafana dashboard surfaces a zero
//     row from idle fleet).
//   - The accessor method on OpsMetrics returns a non-nil
//     Counter for every (app_protocol, bridge_protocol, framing)
//     tuple in the closed sets.
//   - The nil-receiver guard returns nil (matches the other
//     OpsMetrics accessors' nil-safety contract).
//
// Mirrors the shape of TestOpsMetrics_ObserveCounter so a future
// refactor that touches the pre-instantiation pattern fails the
// existing test instead of silently dropping the dashboard row.

package wire_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/wire"
)

// TestOpsMetrics_BridgeFramingTotal_PreInstantiated verifies that
// the full closed cross-product (3 × 2 × 2 = 12 series) appears in
// the /metrics output from boot, before any production request has
// hit the bridge. The /metrics scrape is the upstream source for
// the bridge-protection Grafana dashboard panel.
//
// Reference: ADR-127 §D3 Layer 7 — counter is pre-instantiated at
// NewOpsMetrics so the dashboard renders a zero row from idle
// fleet (§12 panel-at-day-1 contract).
func TestOpsMetrics_BridgeFramingTotal_PreInstantiated(t *testing.T) {
	m := wire.NewOpsMetrics("vmmd")
	srv := httptest.NewServer(m.Handler())
	defer func() { srv.Close() }()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	body := string(bodyBytes)

	wantSubstrings := []string{
		`vmmd_bridge_framing_total{app_protocol="http1",bridge_protocol="h1",framing="match"}`,
		`vmmd_bridge_framing_total{app_protocol="http1",bridge_protocol="h1",framing="mismatch"}`,
		`vmmd_bridge_framing_total{app_protocol="http1",bridge_protocol="h2c",framing="match"}`,
		`vmmd_bridge_framing_total{app_protocol="http1",bridge_protocol="h2c",framing="mismatch"}`,
		`vmmd_bridge_framing_total{app_protocol="http2",bridge_protocol="h1",framing="match"}`,
		`vmmd_bridge_framing_total{app_protocol="http2",bridge_protocol="h1",framing="mismatch"}`,
		`vmmd_bridge_framing_total{app_protocol="http2",bridge_protocol="h2c",framing="match"}`,
		`vmmd_bridge_framing_total{app_protocol="http2",bridge_protocol="h2c",framing="mismatch"}`,
		`vmmd_bridge_framing_total{app_protocol="grpc",bridge_protocol="h1",framing="match"}`,
		`vmmd_bridge_framing_total{app_protocol="grpc",bridge_protocol="h1",framing="mismatch"}`,
		`vmmd_bridge_framing_total{app_protocol="grpc",bridge_protocol="h2c",framing="match"}`,
		`vmmd_bridge_framing_total{app_protocol="grpc",bridge_protocol="h2c",framing="mismatch"}`,
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body missing pre-instantiated series %q", want)
		}
	}
}

// TestOpsMetrics_BridgeFramingTotal_AccessorReturnsCounter verifies
// that BridgeFramingTotal returns a non-nil Counter for every
// tuple in the closed sets. The accessor is the load-bearing
// surface for the producer (cmd/vmmd-stream-bridge::newHandler);
// if it returns nil the counter increment becomes a no-op.
func TestOpsMetrics_BridgeFramingTotal_AccessorReturnsCounter(t *testing.T) {
	m := wire.NewOpsMetrics("vmmd")
	for _, ap := range api.AppProtocolClosedSet {
		for _, bp := range []string{"h1", "h2c"} {
			for _, fr := range []string{"match", "mismatch"} {
				c := m.BridgeFramingTotal(ap, bp, fr)
				if c == nil {
					t.Errorf("BridgeFramingTotal(%q, %q, %q) returned nil", ap, bp, fr)
				}
			}
		}
	}
}

// TestOpsMetrics_BridgeFramingTotal_NilSafe verifies the nil-
// receiver guard. Mirrors the WatchdogKills / WarmSnapshotErrors /
// LivenessRestarts nil-safety contract that allows unit tests to
// run without an OpsMetrics instance.
func TestOpsMetrics_BridgeFramingTotal_NilSafe(t *testing.T) {
	var nilM *wire.OpsMetrics
	if got := nilM.BridgeFramingTotal("http2", "h2c", "match"); got != nil {
		t.Errorf("nil receiver returned non-nil Counter (%T)", got)
	}
}
