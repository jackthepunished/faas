package wire

// Wire-level constants for the cold-wake transparency header (UX spec
// §6, docs/cold-wake.md). The header name is part of the published
// customer contract — it's documented in docs/cold-wake.md and
// docs/faas_ux_spec.md, and tested in pkg/gateway/handler_test.go and
// cmd/gregale/wake_probe_test.go. Do not rename it during a branding
// sweep; the tripwire TestLintTripwire_NoLiteralWakeHeaderOutsidePkgWire
// in cmd/gregale/lint_tripwires_test.go will fail otherwise.
//
// The Gregale rename kept the `x-faas-` prefix on purpose — the wire
// header outlives branding because customers depend on it for devtools
// debugging and the SDK/parity requirements of downstream tooling.
const (
	// WakeHeader is the response header the gateway stamps on a cold
	// wake so the CLI's `gregale open` probe can render the "Waking
	// app" status line.
	WakeHeader = "x-faas-wake"

	// ColdWakeValue is the only value WakeHeader carries that means
	// cold. Exact-match only — "Cold", "cold-start", "true" all
	// resolve to "warm" by probeWakeState.
	ColdWakeValue = "cold"
)
