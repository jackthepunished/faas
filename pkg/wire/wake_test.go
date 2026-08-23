// wake_test.go — fill pkg/wire coverage of the cold-wake wire
// header constants in wake.go. The constants themselves are at 0%
// on the baseline because every production reference to them goes
// through the gatewayd-internal / schedd path (covered indirectly
// at the integration level) — no unit test currently imports the
// literal to assert it stays stable.
//
// These two constants are load-bearing:
//   - WakeHeader is the response header the gateway stamps on a
//     cold wake so the CLI's `gregale open` probe can render the
//     "Waking app" status line. The header name is part of the
//     published customer contract (docs/cold-wake.md,
//     docs/faas_ux_spec.md) and tested in pkg/gateway/handler_test.go
//     and cmd/gregale/wake_probe_test.go. Renaming it would break
//     the tripwire TestLintTripwire_NoLiteralWakeHeaderOutsidePkgWire
//     in cmd/gregale/lint_tripwires_test.go.
//   - ColdWakeValue is the only value WakeHeader carries that
//     means cold. Exact-match only — "Cold", "cold-start", "true"
//     all resolve to "warm" by probeWakeState.

package wire_test

import (
	"testing"

	"github.com/onebox-faas/faas/pkg/wire"
)

// TestWakeHeader_Stable pins the literal wire name. The header is
// part of the published customer contract; renaming it is a breaking
// change for every CLI consumer and dashboard probe.
func TestWakeHeader_Stable(t *testing.T) {
	if got := wire.WakeHeader; got != "x-faas-wake" {
		t.Errorf("WakeHeader = %q, want x-faas-wake", got)
	}
}

// TestColdWakeValue_Stable pins the cold-marker literal. The
// probeWakeState matcher in cmd/gregale/wake_probe.go compares
// against this value byte-for-byte; any drift here silently
// downgrades "cold" responses to "warm" in the CLI status line.
func TestColdWakeValue_Stable(t *testing.T) {
	if got := wire.ColdWakeValue; got != "cold" {
		t.Errorf("ColdWakeValue = %q, want cold", got)
	}
}
