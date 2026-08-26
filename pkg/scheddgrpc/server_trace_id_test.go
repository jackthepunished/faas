// pkg/scheddgrpc/server_trace_id_test.go — PR-#TBD / fix-cluster E.3
// pins for withTraceIDSpan. White-box (package scheddgrpc) so the
// unexported helper can be called directly. Mirrors the
// pkg/middleware/traceid_test.go::TestWithSpanContext_* shape — the
// middleware twin was added in Commit A; this is the scheddgrpc
// twin's coverage.
//
// Three tests pin the contract end-to-end:
//
//   - TestWithTraceIDSpan_StampsOnMetadata: valid 32-char OTel hex
//     on x-faas-trace-id → SpanContext stamped with matching
//     TraceID. The pkg/audit emit path's OTel lift picks it up
//     from the schedd request-side ctx (post-UnaryServerInterceptor
//     chain).
//   - TestWithTraceIDSpan_NilOnEmptyMetadata: no metadata → ctx
//     returned unchanged (no SpanContext stamped).
//   - TestWithTraceIDSpan_NilOnMalformedHex: malformed trace_id
//     → ctx returned unchanged (silent no-op, matches the
//     "garbage-in / no-op" posture of the audit emit's OTel lift).
//
// Important interaction (Commit A's note): the synthetic
// SpanContext this helper stamps has a zero SpanID by design (the
// schedd-side trace_id lift does NOT have a span id — it was
// synthesised from a correlation-id header, not from an inbound
// OTel span). Therefore callers must validate trace_id via
// sc.TraceID().IsValid(), NOT via sc.IsValid() — the latter
// requires both TraceID AND SpanID valid. Audit emit (pkg/audit)
// was updated in Commit A to use the trace-only check.
package scheddgrpc

import (
	"context"
	"testing"

	oteltrace "go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/metadata"
)

const validTraceIDForScheddTests = "4bf92f3577b34da6a3ce929d0e0e4736"

// baseCtxWithTraceMD returns a context carrying a single
// x-faas-trace-id metadata value. Empty value → no metadata key
// at all (matches "no inbound header" semantics, separate from
// "header present but empty").
func baseCtxWithTraceMD(t *testing.T, traceID string) context.Context {
	t.Helper()
	if traceID == "" {
		return context.Background()
	}
	md := metadata.MD{}
	md.Set("x-faas-trace-id", traceID)
	return metadata.NewIncomingContext(context.Background(), md)
}

// TestWithTraceIDSpan_StampsOnMetadata is the happy-path pin:
// a valid 32-char OTel hex on x-faas-trace-id produces a
// SpanContext whose TraceID matches the inbound value.
//
// Note the test asserts on sc.TraceID().IsValid() (NOT
// sc.IsValid()) — see the package doc above. The helper stamps
// a zero SpanID by design; a naive IsValid() check would always
// fail even when the trace_id path is working end-to-end.
func TestWithTraceIDSpan_StampsOnMetadata(t *testing.T) {
	ctx := baseCtxWithTraceMD(t, validTraceIDForScheddTests)
	out := withTraceIDSpan(ctx)

	sc := oteltrace.SpanContextFromContext(out)
	if !sc.TraceID().IsValid() {
		t.Fatalf("TraceID invalid after withTraceIDSpan; expected valid 32-char hex")
	}
	if got := sc.TraceID().String(); got != validTraceIDForScheddTests {
		t.Errorf("TraceID = %q, want %q", got, validTraceIDForScheddTests)
	}
	// SpanID is intentionally zero (no inbound span — just a
	// correlation-id lift). Confirm so a future reviewer who
	// changes the helper to stamp a non-zero SpanID sees the
	// audit-emit TraceID-only check as a now-redundant guard
	// (audit.go:228-239 was already updated in Commit A).
	if sc.SpanID().IsValid() {
		t.Errorf("SpanID = %q, want zero (helper is a trace-only lift, not a span lift)", sc.SpanID().String())
	}
}

// TestWithTraceIDSpan_NilOnEmptyMetadata pins the no-metadata
// short-circuit: when the inbound context carries no
// x-faas-trace-id (the common case for in-process callers), the
// helper returns ctx unchanged with NO SpanContext stamped.
// pkg/audit.Auditor.Emit's OTel lift then sees an invalid
// trace_id and skips the trace_id field on the audit row —
// preserving the pre-PR contract for non-RPC callers.
func TestWithTraceIDSpan_NilOnEmptyMetadata(t *testing.T) {
	ctx := context.Background()
	out := withTraceIDSpan(ctx)

	sc := oteltrace.SpanContextFromContext(out)
	if sc.TraceID().IsValid() {
		t.Errorf("TraceID stamped on context with no metadata; want invalid")
	}
	if sc.SpanID().IsValid() {
		t.Errorf("SpanID stamped on context with no metadata; want invalid")
	}
}

// TestWithTraceIDSpan_NilOnMalformedHex pins the "garbage in /
// no-op" posture. A malformed trace_id (wrong length, non-hex
// chars, etc.) MUST NOT crash the helper or stamp an invalid
// SpanContext — the audit emit path on the other side expects
// the same posture, and a panic in the helper would propagate
// into every schedd RPC handler that goes through the
// trace-id-lift interceptor.
func TestWithTraceIDSpan_NilOnMalformedHex(t *testing.T) {
	for _, bad := range []string{
		"not-hex",                            // non-hex chars
		"4bf92f3577b34da6a3ce",               // too short
		"4bf92f3577b34da6a3ce929d0e0e4736XX", // too long + non-hex
		"GGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGG",   // 32 chars but non-hex
	} {
		t.Run(bad, func(t *testing.T) {
			ctx := baseCtxWithTraceMD(t, bad)
			out := withTraceIDSpan(ctx)
			sc := oteltrace.SpanContextFromContext(out)
			if sc.TraceID().IsValid() {
				t.Errorf("TraceID valid for malformed input %q; want invalid (no-op posture)", bad)
			}
		})
	}
}
