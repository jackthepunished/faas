// Unit tests for the apid SpansWriter gRPC receiver (ADR-127 PR-D).
//
// Coverage:
//   - Valid trace_id + valid JSON + UPDATE succeeds → outcome=inserted.
//   - Bad trace_id regex → InvalidArgument, no UPDATE.
//   - Invalid JSON → InvalidArgument, no UPDATE.
//   - Malformed account_id → Unauthenticated, no UPDATE.
//   - Disabled kill-switch → codes.Unavailable.
//   - DB UPDATE error → codes.Internal.
//   - Rate-limited (bucket exhausted) → outcome=rate_limited, no UPDATE.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	apidpb "github.com/onebox-faas/faas/api/proto/onebox/faas/apid/v1"
	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/ratelimit/peraccount"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// fakeSpansWriterStore substitutes for spansWriterStore in tests.
type fakeSpansWriterStore struct {
	mu       sync.Mutex
	updateFn func(traceID string, summary []byte) error
	calls    int
}

func (f *fakeSpansWriterStore) UpdateSpansSummary(_ context.Context, traceID string, summary []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.updateFn == nil {
		return nil
	}
	return f.updateFn(traceID, summary)
}

// fakeSpansWriterMonitor is a hand-rolled mock for the
// spansWriterMonitor interface the receiver uses.
type fakeSpansWriterMonitor struct {
	mu       sync.Mutex
	outcomes []string
}

func (f *fakeSpansWriterMonitor) IncrementSpansWriteOutcome(outcome string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outcomes = append(f.outcomes, outcome)
}

// dialSpansWriterBufconn brings up an in-process gRPC server backed
// by a bufconn listener and returns a connected SpansWriterClient.
// The listener is cleaned up by t.Cleanup.
func dialSpansWriterBufconn(t *testing.T, store spansWriterStore, ops spansWriterMonitor, limiter *peraccount.Limiter, enabled bool) apidpb.SpansWriterClient {
	t.Helper()
	const bufSize = 1 << 20
	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	registerSpansWriterReceiver(srv, store, ops, limiter, enabled)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { srv.Stop(); _ = lis.Close() })

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithInsecure(),
	)
	if err != nil {
		t.Fatalf("bufconn dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return apidpb.NewSpansWriterClient(conn)
}

func sampleTraceID() string {
	return strings.Repeat("a", 32)
}

func sampleSummary(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"trace_id": sampleTraceID(),
		"spans":    []any{map[string]any{"name": "db.query", "duration_nanos": 191000000}},
	})
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	return b
}

// TestSpansWriter_HappyPath: valid trace_id + valid JSON → inserted.
func TestSpansWriter_HappyPath(t *testing.T) {
	acctID := uuid.New()
	store := &fakeSpansWriterStore{}
	ops := &fakeSpansWriterMonitor{}
	limiter := peraccount.NewLimiter()
	// Pre-warm the limiter with permissive Scale limits so the
	// 1-token take succeeds.
	limiter.CacheLimits(acctID, api.MustLimitsFor(api.PlanScale))
	cli := dialSpansWriterBufconn(t, store, ops, limiter, true)

	resp, err := cli.WriteSpansSummary(context.Background(), &apidpb.WriteSpansSummaryRequest{
		TraceId:     sampleTraceID(),
		SummaryJson: sampleSummary(t),
		AccountId:   acctID.String(),
	})
	if err != nil {
		t.Fatalf("WriteSpansSummary: %v", err)
	}
	if resp.GetOutcome() != "inserted" {
		t.Errorf("outcome = %q, want inserted", resp.GetOutcome())
	}
	if store.calls != 1 {
		t.Errorf("store.calls = %d, want 1", store.calls)
	}
}

// TestSpansWriter_BadTraceID: non-matching regex → InvalidArgument,
// no UPDATE fired.
func TestSpansWriter_BadTraceID(t *testing.T) {
	store := &fakeSpansWriterStore{}
	cli := dialSpansWriterBufconn(t, store, &fakeSpansWriterMonitor{}, peraccount.NewLimiter(), true)

	_, err := cli.WriteSpansSummary(context.Background(), &apidpb.WriteSpansSummaryRequest{
		TraceId:     "not-hex-or-32",
		SummaryJson: sampleSummary(t),
		AccountId:   uuid.New().String(),
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", got)
	}
	if store.calls != 0 {
		t.Errorf("store.calls = %d, want 0", store.calls)
	}
}

// TestSpansWriter_InvalidJSON: bad bytes → InvalidArgument.
func TestSpansWriter_InvalidJSON(t *testing.T) {
	store := &fakeSpansWriterStore{}
	cli := dialSpansWriterBufconn(t, store, &fakeSpansWriterMonitor{}, peraccount.NewLimiter(), true)

	_, err := cli.WriteSpansSummary(context.Background(), &apidpb.WriteSpansSummaryRequest{
		TraceId:     sampleTraceID(),
		SummaryJson: []byte("not json"),
		AccountId:   uuid.New().String(),
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", got)
	}
	if store.calls != 0 {
		t.Errorf("store.calls = %d, want 0", store.calls)
	}
}

// TestSpansWriter_BadAccountID: malformed UUID → Unauthenticated.
func TestSpansWriter_BadAccountID(t *testing.T) {
	store := &fakeSpansWriterStore{}
	cli := dialSpansWriterBufconn(t, store, &fakeSpansWriterMonitor{}, peraccount.NewLimiter(), true)

	_, err := cli.WriteSpansSummary(context.Background(), &apidpb.WriteSpansSummaryRequest{
		TraceId:     sampleTraceID(),
		SummaryJson: sampleSummary(t),
		AccountId:   "not-a-uuid",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Errorf("code = %v, want Unauthenticated", got)
	}
	if store.calls != 0 {
		t.Errorf("store.calls = %d, want 0", store.calls)
	}
}

// TestSpansWriter_Disabled: kill-switch → codes.Unavailable.
func TestSpansWriter_Disabled(t *testing.T) {
	store := &fakeSpansWriterStore{}
	cli := dialSpansWriterBufconn(t, store, &fakeSpansWriterMonitor{}, peraccount.NewLimiter(), false)

	_, err := cli.WriteSpansSummary(context.Background(), &apidpb.WriteSpansSummaryRequest{
		TraceId:     sampleTraceID(),
		SummaryJson: sampleSummary(t),
		AccountId:   uuid.New().String(),
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := status.Code(err); got != codes.Unavailable {
		t.Errorf("code = %v, want Unavailable", got)
	}
}

// TestSpansWriter_DBError: store.UpdateSpansSummary returns an
// arbitrary error → codes.Internal.
func TestSpansWriter_DBError(t *testing.T) {
	store := &fakeSpansWriterStore{
		updateFn: func(_ string, _ []byte) error {
			return errors.New("postgres down")
		},
	}
	cli := dialSpansWriterBufconn(t, store, &fakeSpansWriterMonitor{}, peraccount.NewLimiter(), true)

	_, err := cli.WriteSpansSummary(context.Background(), &apidpb.WriteSpansSummaryRequest{
		TraceId:     sampleTraceID(),
		SummaryJson: sampleSummary(t),
		AccountId:   uuid.New().String(),
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := status.Code(err); got != codes.Internal {
		t.Errorf("code = %v, want Internal", got)
	}
}

// TestSpansWriter_RateLimited: bucket cap=1 → second call
// returns outcome=rate_limited without firing UPDATE.
func TestSpansWriter_RateLimited(t *testing.T) {
	acctID := uuid.New()
	store := &fakeSpansWriterStore{}
	limiter := peraccount.NewLimiter()
	limiter.CacheLimits(acctID, api.Limits{DebugTelemetryRequestsPerMinute: 1})
	cli := dialSpansWriterBufconn(t, store, &fakeSpansWriterMonitor{}, limiter, true)

	req := &apidpb.WriteSpansSummaryRequest{
		TraceId:     sampleTraceID(),
		SummaryJson: sampleSummary(t),
		AccountId:   acctID.String(),
	}
	resp1, err := cli.WriteSpansSummary(context.Background(), req)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if resp1.GetOutcome() != "inserted" {
		t.Errorf("first outcome = %q, want inserted", resp1.GetOutcome())
	}
	resp2, err := cli.WriteSpansSummary(context.Background(), req)
	if err != nil {
		t.Fatalf("second call: %v (rate-limit should NOT surface as gRPC error)", err)
	}
	if resp2.GetOutcome() != "rate_limited" {
		t.Errorf("second outcome = %q, want rate_limited", resp2.GetOutcome())
	}
	if resp2.GetRetryAfterMs() <= 0 {
		t.Errorf("second retry_after_ms = %d, want > 0", resp2.GetRetryAfterMs())
	}
	if store.calls != 1 {
		t.Errorf("store.calls = %d, want 1 (rate-limited MUST NOT fire UPDATE)", store.calls)
	}
}
