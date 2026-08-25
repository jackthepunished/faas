// pkg/audit/audit_trace_id_test.go — PR-#TBD / C4 pins the audit
// emit path's trace_id lift into the events table.
//
// Pins:
//
//  1. Emit lifts data["trace_id"] onto the events.trace_id column
//     (C4 + C2 contract).
//  2. Emit leaves events.trace_id NULL when data has no
//     "trace_id" key (pre-PR contract preserved).
//  3. Emit respects an explicit data["trace_id"] over the OTel
//     span-context lift (matches the existing rule at
//     pkg/audit/audit.go:221-232).
//  4. Emit passes nil (not pointer to empty string) when data's
//     "trace_id" is the empty string — the column is nullable.
//
// Build tag: no Postgres required; tests use MemStore which
// mirrors the column read/write shape via AppendEventWithTrace.

package audit_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/onebox-faas/faas/pkg/audit"
	"github.com/onebox-faas/faas/pkg/state"
)

// TestEmit_TraceIDLiftedToColumn pins the C4 contract: an
// explicit data["trace_id"] is persisted on the events row's
// TraceID field via AppendEventWithTrace.
func TestEmit_TraceIDLiftedToColumn(t *testing.T) {
	ctx := context.Background()
	store := state.NewMemStore()
	auditor := audit.New(store, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, "test:apid")

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	auditor.Emit(ctx, "test.kind", nil, map[string]any{
		"k":        "v",
		"trace_id": traceID,
	})

	events, err := store.ListEvents(ctx, "", 100)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	got := events[0]
	if got.TraceID == nil {
		t.Fatalf("Event.TraceID = nil; want %q", traceID)
	}
	if *got.TraceID != traceID {
		t.Errorf("Event.TraceID = %q, want %q", *got.TraceID, traceID)
	}
}

// TestEmit_NoTraceIDLeavesColumnNil pins the pre-PR contract for
// sites that don't author a trace_id (e.g. cron-fired rows, legacy
// emit sites).
func TestEmit_NoTraceIDLeavesColumnNil(t *testing.T) {
	ctx := context.Background()
	store := state.NewMemStore()
	auditor := audit.New(store, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, "test:apid")

	auditor.Emit(ctx, "test.kind", nil, map[string]any{"k": "v"})

	events, err := store.ListEvents(ctx, "", 100)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	if events[0].TraceID != nil {
		t.Errorf("Event.TraceID = %v, want nil", *events[0].TraceID)
	}
}

// TestEmit_ExplicitTraceIDWinsOverSpanContext pins the
// precedence rule. The OTel span-context lift at audit.go:221-232
// runs BEFORE the trace_id column extract at C4; both write the
// same key on data, so the second writer loses. C4's extract
// intentionally runs after the OTel lift to preserve the
// "explicit value wins" contract.
func TestEmit_ExplicitTraceIDWinsOverSpanContext(t *testing.T) {
	ctx := context.Background()
	store := state.NewMemStore()
	auditor := audit.New(store, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, "test:apid")

	spanCtx := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0xaa, 0xbb, 0xcc, 0xdd, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0x00, 0xaa, 0xbb},
		SpanID:     trace.SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	otelCtx := trace.ContextWithSpanContext(ctx, spanCtx)

	const explicit = "4bf92f3577b34da6a3ce929d0e0e4736"
	auditor.Emit(otelCtx, "test.kind", nil, map[string]any{
		"trace_id": explicit,
	})

	events, err := store.ListEvents(ctx, "", 100)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	if events[0].TraceID == nil || *events[0].TraceID != explicit {
		t.Errorf("Event.TraceID = %v, want %q (explicit must win over span context)", events[0].TraceID, explicit)
	}
}

// TestEmit_EmptyStringTraceIDLeavesColumnNil pins the empty-string
// normalisation. data["trace_id"] == "" is treated as absent so
// the column is NULL — protects against future callers that
// accidentally set "" instead of nil.
func TestEmit_EmptyStringTraceIDLeavesColumnNil(t *testing.T) {
	ctx := context.Background()
	store := state.NewMemStore()
	auditor := audit.New(store, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, "test:apid")

	auditor.Emit(ctx, "test.kind", nil, map[string]any{
		"trace_id": "",
	})

	events, err := store.ListEvents(ctx, "", 100)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	if events[0].TraceID != nil {
		t.Errorf("Event.TraceID = %v, want nil (empty string should be treated as absent)", *events[0].TraceID)
	}
}