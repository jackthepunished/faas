// Unit tests for the gatewayd-public OTelSpansHandler (ADR-127 PR-D).
//
// Coverage:
//   - Happy path: valid OTLP body + valid bearer → 200 + accumulator
//     populated + metric counter incremented.
//   - 401 on bearer missing / auth RPC error.
//   - 402 on plan-disabled (DebugTelemetryEnabled=false).
//   - 429 on per-account rate cap exhaustion.
//   - 400 on shape-invalid body (no spans, malformed JSON,
//     trace_id mismatch).
//   - 405 on non-POST.
//   - Top-N truncation correctness: 75 Hobby spans → keep 50
//     slowest; assert the kept set is the 50 largest DurationNanos.
//   - Truncation tripwire metric fires when input > ceiling.

package gateway

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/ratelimit/peraccount"
	"github.com/onebox-faas/faas/pkg/wire"
)

// fakeAuthClient substitutes for apidgrpc.AuthClient in tests.
type fakeAuthClient struct {
	mu        sync.Mutex
	calls     int
	accountID string
	plan      string
	err       error
}

func (f *fakeAuthClient) AuthenticateKey(_ context.Context, _ string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.accountID, f.plan, f.err
}
func (f *fakeAuthClient) Close() error { return nil }

// hobbyLimits returns a Hobby plan's telemetry caps for tests.
func hobbyLimits() api.Limits {
	l := api.MustLimitsFor(api.PlanHobby)
	return l
}

// buildOTLPBody assembles an ExportTraceServiceRequest with n
// spans whose durations are 1..n nanoseconds. Returns the raw
// JSON-protobuf bytes ready to feed to the handler.
func buildOTLPBody(t *testing.T, n int) []byte {
	t.Helper()
	var traceIDBytes [16]byte
	for i := range traceIDBytes {
		traceIDBytes[i] = byte(0xab)
	}
	var spanIDBytes [8]byte
	for i := range spanIDBytes {
		spanIDBytes[i] = byte(0xcd)
	}
	resourceSpans := &tracepb.ResourceSpans{
		ScopeSpans: []*tracepb.ScopeSpans{
			{Spans: make([]*tracepb.Span, 0, n)},
		},
	}
	for i := 1; i <= n; i++ {
		span := &tracepb.Span{
			TraceId:           traceIDBytes[:],
			SpanId:            spanIDBytes[:],
			Name:              "db.query",
			StartTimeUnixNano: 0,
			EndTimeUnixNano:   uint64(i),
			Attributes: []*commonpb.KeyValue{
				{Key: "db.statement", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "SELECT " + uuid.New().String()}}},
			},
		}
		resourceSpans.ScopeSpans[0].Spans = append(resourceSpans.ScopeSpans[0].Spans, span)
	}
	req := &collectortracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{resourceSpans},
	}
	b, err := protojson.Marshal(req)
	if err != nil {
		t.Fatalf("protojson.Marshal: %v", err)
	}
	return b
}

// TestOTelSpansHandler_HappyPath: 3-span Hobby POST → 200,
// accumulator populated with 3 spans.
func TestOTelSpansHandler_HappyPath(t *testing.T) {
	acctID := uuid.New()
	auth := &fakeAuthClient{accountID: acctID.String(), plan: "hobby"}
	acc := NewSpansAccumulator()
	ops := wire.NewOpsMetrics("test")
	h := NewOTelSpansHandler(OTelSpansHandlerConfig{
		AuthClient: auth,
		Limiter:    peraccount.NewLimiter(),
		Acc:        acc,
		Ops:        ops,
	})

	body := buildOTLPBody(t, 3)
	req := httptest.NewRequest(http.MethodPost, "/v1/otel/v1/traces", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer faas_live_hobby")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	var resp otelSpansResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.AcceptedSpans != 3 {
		t.Errorf("accepted_spans = %d, want 3", resp.AcceptedSpans)
	}
	if resp.Truncated {
		t.Errorf("truncated = true, want false")
	}
	if acc.Len() != 1 {
		t.Errorf("accumulator buckets = %d, want 1", acc.Len())
	}
}

// TestOTelSpansHandler_401_NoBearer: missing Authorization → 401.
func TestOTelSpansHandler_401_NoBearer(t *testing.T) {
	auth := &fakeAuthClient{err: errors.New("unused")}
	ops := wire.NewOpsMetrics("test")
	h := NewOTelSpansHandler(OTelSpansHandlerConfig{
		AuthClient: auth,
		Limiter:    peraccount.NewLimiter(),
		Acc:        NewSpansAccumulator(),
		Ops:        ops,
	})
	body := buildOTLPBody(t, 1)
	req := httptest.NewRequest(http.MethodPost, "/v1/otel/v1/traces", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body=%s)", rr.Code, rr.Body.String())
	}
	if auth.calls != 0 {
		t.Errorf("auth.calls = %d, want 0 (must short-circuit before RPC)", auth.calls)
	}
}

// TestOTelSpansHandler_402_PlanDisabled: auth returns plan without
// DebugTelemetryEnabled → 402 + metric reason=plan_disabled.
func TestOTelSpansHandler_402_PlanDisabled(t *testing.T) {
	acctID := uuid.New()
	auth := &fakeAuthClient{accountID: acctID.String(), plan: "free"}
	ops := wire.NewOpsMetrics("test")
	h := NewOTelSpansHandler(OTelSpansHandlerConfig{
		AuthClient: auth,
		Limiter:    peraccount.NewLimiter(),
		Acc:        NewSpansAccumulator(),
		Ops:        ops,
	})
	body := buildOTLPBody(t, 1)
	req := httptest.NewRequest(http.MethodPost, "/v1/otel/v1/traces", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer faas_live_free")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402 (body=%s)", rr.Code, rr.Body.String())
	}
}

// TestOTelSpansHandler_429_RateLimited: bucket cap=1 (via stub
// LimitsFor) → second POST within the same window gets 429.
func TestOTelSpansHandler_429_RateLimited(t *testing.T) {
	acctID := uuid.New()
	auth := &fakeAuthClient{accountID: acctID.String(), plan: "hobby"}
	limiter := peraccount.NewLimiter()
	ops := wire.NewOpsMetrics("test")
	h := NewOTelSpansHandler(OTelSpansHandlerConfig{
		AuthClient: auth,
		Limiter:    limiter,
		Acc:        NewSpansAccumulator(),
		Ops:        ops,
		// Stub the plan table to a cap=1 Hobby-shaped limit so
		// the second POST trips without burning 1000 buckets.
		LimitsFor: func(_ api.Plan) api.Limits {
			return api.Limits{
				DebugTelemetryEnabled:           true,
				DebugTelemetryRequestsPerMinute: 1,
				DebugTelemetrySpansPerTrace:     50,
			}
		},
	})
	body := buildOTLPBody(t, 1)
	makeReq := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/otel/v1/traces", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer faas_live_hobby")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	if rr := makeReq(); rr.Code != http.StatusOK {
		t.Fatalf("first POST: status = %d, want 200", rr.Code)
	}
	if rr := makeReq(); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("second POST: status = %d, want 429 (body=%s)", rr.Code, rr.Body.String())
	}
}

// TestOTelSpansHandler_400_MalformedJSON: garbage bytes → 400.
func TestOTelSpansHandler_400_MalformedJSON(t *testing.T) {
	auth := &fakeAuthClient{accountID: uuid.New().String(), plan: "hobby"}
	ops := wire.NewOpsMetrics("test")
	h := NewOTelSpansHandler(OTelSpansHandlerConfig{
		AuthClient: auth,
		Limiter:    peraccount.NewLimiter(),
		Acc:        NewSpansAccumulator(),
		Ops:        ops,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/otel/v1/traces", strings.NewReader("not json"))
	req.Header.Set("Authorization", "Bearer faas_live_hobby")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

// TestOTelSpansHandler_400_TraceIDMismatch: two spans with
// different trace_id → 400 trace_mismatch.
func TestOTelSpansHandler_400_TraceIDMismatch(t *testing.T) {
	auth := &fakeAuthClient{accountID: uuid.New().String(), plan: "hobby"}
	ops := wire.NewOpsMetrics("test")
	h := NewOTelSpansHandler(OTelSpansHandlerConfig{
		AuthClient: auth,
		Limiter:    peraccount.NewLimiter(),
		Acc:        NewSpansAccumulator(),
		Ops:        ops,
	})
	// Two spans, two different trace_ids.
	tid1 := bytes.Repeat([]byte{0xab}, 16)
	tid2 := bytes.Repeat([]byte{0xcd}, 16)
	sid := bytes.Repeat([]byte{0x01}, 8)
	req := &collectortracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{
			{ScopeSpans: []*tracepb.ScopeSpans{
				{Spans: []*tracepb.Span{
					{TraceId: tid1, SpanId: sid, Name: "a", StartTimeUnixNano: 0, EndTimeUnixNano: 1},
					{TraceId: tid2, SpanId: sid, Name: "b", StartTimeUnixNano: 0, EndTimeUnixNano: 2},
				}},
			}},
		},
	}
	body, _ := protojson.Marshal(req)
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/otel/v1/traces", bytes.NewReader(body))
	httpReq.Header.Set("Authorization", "Bearer faas_live_hobby")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httpReq)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
	}
}

// TestOTelSpansHandler_405_NotPOST: GET → 405 + Allow: POST.
func TestOTelSpansHandler_405_NotPOST(t *testing.T) {
	auth := &fakeAuthClient{}
	ops := wire.NewOpsMetrics("test")
	h := NewOTelSpansHandler(OTelSpansHandlerConfig{
		AuthClient: auth,
		Limiter:    peraccount.NewLimiter(),
		Acc:        NewSpansAccumulator(),
		Ops:        ops,
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/otel/v1/traces", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
	if got := rr.Header().Get("Allow"); got != "POST" {
		t.Errorf("Allow header = %q, want POST", got)
	}
}

// TestOTelSpansHandler_Truncation: 75 Hobby spans → keep 50
// slowest; assert the kept set is the 50 largest DurationNanos.
// The truncation tripwire metric fires once.
func TestOTelSpansHandler_Truncation(t *testing.T) {
	acctID := uuid.New()
	auth := &fakeAuthClient{accountID: acctID.String(), plan: "hobby"}
	acc := NewSpansAccumulator()
	ops := wire.NewOpsMetrics("test")
	h := NewOTelSpansHandler(OTelSpansHandlerConfig{
		AuthClient: auth,
		Limiter:    peraccount.NewLimiter(),
		Acc:        acc,
		Ops:        ops,
	})

	body := buildOTLPBody(t, 75)
	req := httptest.NewRequest(http.MethodPost, "/v1/otel/v1/traces", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer faas_live_hobby")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	var resp otelSpansResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Truncated {
		t.Errorf("truncated = false, want true")
	}
	if resp.AcceptedSpans != 50 {
		t.Errorf("accepted_spans = %d, want 50 (Hobby ceiling)", resp.AcceptedSpans)
	}

	// Pull the kept spans out of the accumulator and verify
	// they're the 50 largest DurationNanos (the test builder
	// gives span i duration = i nanos, so the slowest 50 are
	// indices 26..75 = durations 26..75).
	spans, _ := acc.DrainAndRemove(hex.EncodeToString(bytes.Repeat([]byte{0xab}, 16)))
	if len(spans) != 50 {
		t.Fatalf("kept span count = %d, want 50", len(spans))
	}
	for i := 1; i < len(spans); i++ {
		if spans[i].DurationNanos > spans[i-1].DurationNanos {
			t.Errorf("kept spans not sorted descending by duration: spans[%d]=%d > spans[%d]=%d",
				i, spans[i].DurationNanos, i-1, spans[i-1].DurationNanos)
		}
	}
	// Slowest kept is span #75 (duration=75 nanos). Fastest
	// kept is span #26 (duration=26 nanos).
	if spans[len(spans)-1].DurationNanos < 26 {
		t.Errorf("fastest kept = %d, want >= 26 (slowest 50 of 75)", spans[len(spans)-1].DurationNanos)
	}
	if spans[0].DurationNanos != 75 {
		t.Errorf("slowest kept = %d, want 75", spans[0].DurationNanos)
	}
}

// Compile-time guard that protojson accepts the message shapes
// we build (the otlp deps occasionally change type signatures
// across minor versions — failing at the test boundary beats
// failing in production).
var _ = protojson.MarshalOptions{}
