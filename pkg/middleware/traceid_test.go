// pkg/middleware/traceid_test.go — PR-#TBD / C4 pins the TraceID
// middleware contract.
//
// Pins:
//
//  1. TraceID generates a fresh OTel hex when the inbound
//     X-Trace-Id header is absent.
//  2. TraceID respects an inbound X-Trace-Id header and echoes
//     the same value on the response.
//  3. WithTraceID stores the value on the context; TraceIDFrom
//     reads it back.
//  4. WithTraceID with an empty id is a no-op (preserves the
//     pre-existing context).
//  5. TraceIDFrom on a nil *http.Request returns a fresh value
//     (defensive — the middleware always has a non-nil request,
//     but downstream helpers may be called with nil in tests).
//  6. TraceIDFrom returns nil when no middleware ran AND no
//     inbound header is set — pre-PR contract for cron-fired
//     sites that build a *http.Request via httptest.NewRequest
//     and pass through.

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	oteltrace "go.opentelemetry.io/otel/trace"
)

// TestTraceID_GeneratesWhenInboundAbsent pins the generation
// path. Pattern mirrors pkg/middleware/requestid_test.go's
// TestRequestID_GeneratesWhenAbsent.
func TestTraceID_GeneratesWhenInboundAbsent(t *testing.T) {
	var got *string
	var ctxValue string
	var sc oteltrace.SpanContext
	h := TraceID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = TraceIDFrom(r)
		if v, ok := r.Context().Value(TraceIDKey{}).(string); ok {
			ctxValue = v
		}
		// PR-#TBD / fix-cluster A — the middleware must also
		// stamp a valid OTel SpanContext on r.Context() so
		// downstream audit.Auditor.Emit's OTel-lift finds the
		// trace_id via oteltrace.SpanContextFromContext. Without
		// this, the apid audit emit's trace_id lands as NULL
		// even though TraceIDKey was set.
		sc = oteltrace.SpanContextFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got == nil {
		t.Fatalf("TraceIDFrom returned nil; want non-nil (middleware should always set one)")
	}
	if len(*got) != 32 {
		t.Errorf("TraceIDFrom length = %d, want 32 (OTel hex)", len(*got))
	}
	if ctxValue != *got {
		t.Errorf("context value %q != TraceIDFrom() %q", ctxValue, *got)
	}
	if h := rr.Header().Get(TraceIDHeader); h != *got {
		t.Errorf("response header %q != TraceIDFrom() %q", h, *got)
	}
	if !sc.TraceID().IsValid() {
		t.Errorf("OTel SpanContext.TraceID().IsValid() = false; want true (middleware must bridge TraceIDKey → SpanContext)")
	}
	if sc.TraceID().String() != *got {
		t.Errorf("SpanContext.TraceID() = %q, want %q (match TraceIDKey value)", sc.TraceID().String(), *got)
	}
}

// TestTraceID_RespectsInboundHeader pins the override path.
func TestTraceID_RespectsInboundHeader(t *testing.T) {
	const inbound = "4bf92f3577b34da6a3ce929d0e0e4736"
	var got *string
	h := TraceID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = TraceIDFrom(r)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(TraceIDHeader, inbound)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got == nil || *got != inbound {
		t.Errorf("TraceIDFrom = %v, want %q", got, inbound)
	}
	if h := rr.Header().Get(TraceIDHeader); h != inbound {
		t.Errorf("response header %q != inbound %q", h, inbound)
	}
}

// TestTraceID_HeaderIsCaseInsensitive confirms the http.Header
// lookup uses canonical form and accepts lowercase variants.
func TestTraceID_HeaderIsCaseInsensitive(t *testing.T) {
	const inbound = "4bf92f3577b34da6a3ce929d0e0e4736"
	var got *string
	h := TraceID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = TraceIDFrom(r)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("x-trace-id", inbound) // lowercase
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got == nil || *got != inbound {
		t.Errorf("TraceIDFrom with lowercase header = %v, want %q", got, inbound)
	}
}

// TestWithTraceID_NilOnEmpty confirms the empty-id no-op.
func TestWithTraceID_NilOnEmpty(t *testing.T) {
	ctx := context.Background()
	got := WithTraceID(ctx, "")
	if got != ctx {
		t.Errorf("WithTraceID(ctx, \"\") returned %v, want unchanged ctx", got)
	}
}

// TestTraceIDFrom_NoMiddlewareNoHeaderReturnsNil pins the
// pre-PR contract: a request that has not been through TraceID
// middleware AND has no inbound header returns nil — so callers
// can distinguish "trace_id was supplied" from "no trace_id".
func TestTraceIDFrom_NoMiddlewareNoHeaderReturnsNil(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	if got := TraceIDFrom(req); got != nil {
		t.Errorf("TraceIDFrom (no middleware, no header) = %v, want nil", got)
	}
}

// TestTraceIDFrom_NilRequestReturnsFresh pins the nil-safety
// contract. A fresh value is returned (not nil) so callers don't
// have to nil-check, and the value is still the OTel regex shape.
func TestTraceIDFrom_NilRequestReturnsFresh(t *testing.T) {
	got := TraceIDFrom(nil)
	if got == nil {
		t.Fatalf("TraceIDFrom(nil) = nil; want fresh value")
	}
	if len(*got) != 32 {
		t.Errorf("TraceIDFrom(nil) length = %d, want 32", len(*got))
	}
}

// TestNewTraceID_Format pins the OTel regex shape.
func TestNewTraceID_Format(t *testing.T) {
	for i := 0; i < 100; i++ {
		s := NewTraceID()
		if len(s) != 32 {
			t.Fatalf("NewTraceID length = %d, want 32", len(s))
		}
		for j := 0; j < len(s); j++ {
			c := s[j]
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				t.Errorf("NewTraceID[%d] = %q; want lowercase hex", j, c)
			}
		}
	}
}

// TestWithSpanContext_RoundTrip pins the happy path: a valid
// 32-char OTel hex trace_id lifts into a SpanContext whose
// TraceID matches the input. The synthetic SpanContext carries
// a valid TraceID + zero SpanID (no real tracer is involved);
// the audit emit's OTel-lift at pkg/audit/audit.go:228-239 reads
// trace_id via sc.TraceID().IsValid() (which IS true here) so
// the bridge is load-bearing even though SpanContext.IsValid()
// returns false (SpanID is zero).
func TestWithSpanContext_RoundTrip(t *testing.T) {
	const id = "4bf92f3577b34da6a3ce929d0e0e4736"
	ctx := WithSpanContext(context.Background(), id)
	sc := oteltrace.SpanContextFromContext(ctx)
	if !sc.TraceID().IsValid() {
		t.Fatalf("SpanContext.TraceID().IsValid() = false; want true (valid OTel hex)")
	}
	if got := sc.TraceID().String(); got != id {
		t.Errorf("SpanContext.TraceID() = %q, want %q", got, id)
	}
	if sc.SpanID().IsValid() {
		t.Errorf("SpanContext.SpanID() = %q; want zero SpanID (synthetic ctx only carries trace_id)", sc.SpanID())
	}
	if sc.TraceFlags() != oteltrace.FlagsSampled {
		t.Errorf("SpanContext.TraceFlags() = %v; want FlagsSampled", sc.TraceFlags())
	}
	if !sc.IsRemote() {
		t.Errorf("SpanContext.IsRemote() = false; want true (synthetic ctx is remote)")
	}
}

// TestWithSpanContext_NilOnEmpty pins the empty-id no-op. Mirrors
// WithTraceID's empty-id contract so callers can compose them
// without nil-checking.
func TestWithSpanContext_NilOnEmpty(t *testing.T) {
	ctx := context.Background()
	got := WithSpanContext(ctx, "")
	if got != ctx {
		t.Errorf("WithSpanContext(ctx, \"\") returned %v, want unchanged ctx", got)
	}
	if sc := oteltrace.SpanContextFromContext(got); sc.TraceID().IsValid() {
		t.Errorf("WithSpanContext(ctx, \"\") stamped a valid TraceID; want invalid")
	}
}

// TestWithSpanContext_NilCtx confirms the nil-ctx no-op. Defensive
// — the middleware always passes a non-nil ctx, but downstream
// helpers may pass through nil.
func TestWithSpanContext_NilCtx(t *testing.T) {
	got := WithSpanContext(nil, "4bf92f3577b34da6a3ce929d0e0e4736")
	if got != nil {
		t.Errorf("WithSpanContext(nil, id) = %v, want nil", got)
	}
}

// TestWithSpanContext_MalformedHex pins the "garbage-in / no-op"
// posture. A malformed inbound trace_id (non-hex chars, wrong
// length) does NOT poison the SpanContext — the audit emit's
// own OTel lift has the same posture.
func TestWithSpanContext_MalformedHex(t *testing.T) {
	cases := []struct {
		name string
		id   string
	}{
		{"non-hex-chars", "not-a-hex-value-but-32charsXX"},
		{"too-short", "4bf92f3577b34da6a3ce929d0e0e473"},
		{"too-long", "4bf92f3577b34da6a3ce929d0e0e473600"},
		{"uppercase-rejected", "4BF92F3577B34DA6A3CE929D0E0E4736"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := WithSpanContext(context.Background(), tc.id)
			if sc := oteltrace.SpanContextFromContext(ctx); sc.TraceID().IsValid() {
				t.Errorf("malformed id %q stamped a valid TraceID %q; want invalid", tc.id, sc.TraceID())
			}
		})
	}
}
