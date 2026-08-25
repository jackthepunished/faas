// Package middleware — pkg/middleware/traceid.go (PR-#TBD / C4).
//
// Middleware that ensures every inbound request carries an OTel
// W3C 32-char hex trace_id (header `X-Trace-Id`, case-insensitive
// per http.Header conventions). Mirrors pkg/middleware/requestid.go
// (the request-id sibling at x-faas-request-id) but writes a
// DIFFERENT value — request-id is a uuid-style 128-bit hex used
// for log correlation; trace_id is the OTel W3C format that the
// operator-action observability layer joins on. They are
// semantically distinct even though the wire format is similar.
//
// Inbound header convention:
//
//	X-Trace-Id: <32 lowercase hex chars>
//
// Header name uses HTTP canonical form. http.Header.Get is
// case-insensitive; tests use the canonical "X-Trace-Id" form.
//
// Why a separate middleware (not a flag on RequestID):
//
//   - The request-id surface is stable across daemons
//     (x-faas-request-id has been there since M0). Wiring trace-id
//     into the same chain risks regressing the legacy surface for
//     a feature that not every daemon needs yet.
//   - C4 wires TraceID only on the apid admin force-action chain
//     (cmd/apid/server.go:1459-1499); C6 will add it on the gRPC
//     inbound path. Out of scope: dashboardChain / cliAuthChain.
//
// Storage on the request context (TraceIDKey{}) lets handlers
// pull the value via middleware.TraceIDFrom(r). Downstream the
// value flows through InsertOperatorIntent → operator_intents.
// trace_id (C2) → schedd subscriber → audit emit data["trace_id"]
// (C3) → pkg/audit.emit → events.trace_id column (C4 — the final
// step).
package middleware

import (
	"context"
	"net/http"

	"github.com/onebox-faas/faas/pkg/wire"
)

// TraceIDHeader is the canonical case of the inbound/outbound
// header. http.Header.Get is case-insensitive so the GET checks
// tolerate X-Trace-Id, x-trace-id, etc.
const TraceIDHeader = "X-Trace-Id"

// TraceIDKey is the context key for the per-request trace id.
// Exported so handlers can pull the value via TraceIDFrom.
// Empty string is a no-op set.
type TraceIDKey struct{}

// WithTraceID stores id on ctx and returns the new context. Empty
// id is a no-op so callers can pass through whatever they got.
func WithTraceID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, TraceIDKey{}, id)
}

// TraceIDFrom extracts the trace id from r's context, falling
// back to the X-Trace-Id request header set upstream, and finally
// to a fresh OTel hex via wire.NewTraceID(). nil-safe.
//
// Mirrors the precedence of pkg/middleware.RequestIDFrom: ctx
// value wins (so a daemon-internal mid-chain hop can override the
// inbound header), then the header, then a fresh value. Returns
// *string so callers can distinguish "no trace_id supplied" (nil)
// from "trace_id is the zero string" — pre-PR rows + cron-fired
// rows without an inbound trace_id keep nil and land as NULL in
// the events.trace_id / operator_intents.trace_id columns.
func TraceIDFrom(r *http.Request) *string {
	if r == nil {
		s := wire.NewTraceID()
		return &s
	}
	if v, ok := r.Context().Value(TraceIDKey{}).(string); ok && v != "" {
		return &v
	}
	if v := r.Header.Get(TraceIDHeader); v != "" {
		return &v
	}
	return nil // pre-PR convention: nil when no trace_id supplied.
}

// NewTraceID returns a fresh OTel W3C 32-char hex trace_id.
// Thin re-export of pkg/wire.NewTraceID so the middleware
// package surface stays self-contained.
func NewTraceID() string {
	return wire.NewTraceID()
}

// TraceID is a middleware that ensures every inbound request
// has an X-Trace-Id header (inbound override OR freshly generated),
// stores it on the context via WithTraceID, and echoes it back
// on the response. Wire-compatible with the OTel W3C traceparent
// header convention (without the version + parent-id fields —
// the canonical OTel carrier is traceparent: 00-<trace_id>-<span_id>-01,
// but we operate one level below that: just the trace_id column
// in our schema). A future sibling PR may upgrade to full
// traceparent if/when OTel exporters come online.
func TraceID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Inbound override takes precedence. Generate fresh when
		// absent. The OTel regex (^[0-9a-f]{32}$) is enforced on
		// the database side; a malformed inbound value would
		// surface as SQLSTATE 23514 at AppendEventWithTrace time
		// — we trust the writer and don't validate here to keep
		// the middleware path fast.
		traceID := r.Header.Get(TraceIDHeader)
		if traceID == "" {
			traceID = NewTraceID()
		}
		w.Header().Set(TraceIDHeader, traceID)
		r = r.WithContext(WithTraceID(r.Context(), traceID))
		next.ServeHTTP(w, r)
	})
}