// Package auditutil is the cross-daemon helper seam for audit-event
// payload shaping. The audit pipeline writes `events` rows through
// two distinct emitters — pkg/audit.Auditor (apid / schedd / meterd /
// githubd / gregale) and cmd/gatewayd-internal/audit.go::gatewaydAuditor
// (the proxy's webhook / edge-rule surfaces) — and both need a single
// source of truth for payload conventions like the `result` field
// (PR-B residual). This package is that source.
//
// WithResult is the only helper today. It stamps
// `result: "success"|"error"` onto a data map so every audit-event
// row carries the outcome without the call site having to repeat
// the literal. The helper is a no-op when:
//   - data is nil (it allocates an empty map so the caller can keep
//     a single code path: `data := WithResult(payload, "success")`).
//   - result is "" (the caller has no meaningful result to stamp —
//     keeps pre-existing payload shape intact).
//   - data already has a "result" key (caller's explicit value wins
//     so an in-line override is never silently overwritten).
//
// Adding future cross-auditor payload conventions (e.g. a "severity"
// field per the operator-obs follow-on ADR-092) belongs in this
// package rather than being inlined at either Emit method.
package auditutil

// WithResult returns data with result:"success"|"error" stamped in.
//
// Behaviour:
//   - nil data → allocated empty map (so the caller's single-line
//     `data := WithResult(payload, result)` shape is safe even when
//     payload is nil).
//   - empty result → data is returned unchanged (no field stamped).
//   - data["result"] already set → data is returned unchanged
//     (caller's explicit value wins).
//   - otherwise → data["result"] = result, data is returned.
//
// The function does NOT defensively copy the input map. Callers
// that pass a shared map and mutate it after the call will see the
// stamped value — the same contract as pkg/audit.Auditor.Emit, which
// already mutates its data parameter to inject trace_id / span_id.
func WithResult(data map[string]any, result string) map[string]any {
	if data == nil {
		data = map[string]any{}
	}
	if result == "" {
		return data
	}
	if _, ok := data["result"]; ok {
		return data
	}
	data["result"] = result
	return data
}
