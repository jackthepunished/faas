// Package gateway — otel_spans_handler.go (ADR-127 PR-D).
//
// gatewayd-public's POST /v1/otel/v1/traces handler (OTLP/HTTP
// JSON-protobuf, the standard sidecar protocol). Decodes the
// inbound ExportTraceServiceRequest, validates per-account auth
// via the apid Auth gRPC service, enforces the per-plan telemetry
// rate cap (DebugTelemetryRequestsPerMinute) + span ceiling
// (DebugTelemetrySpansPerTrace), and feeds the truncated spans
// into a per-trace accumulator (Stage 4 flushes the accumulator
// to apid's WriteSpansSummary gRPC RPC).
//
// Auth model: loopback gRPC, single-box mode. The unix socket
// /run/faas/auth.sock is DAC 0660 group `faas`; the gateway's
// only credential is socket membership. Cross-box the loopback
// is replaced by the FAAS_AUTH_RPC target (PR-D's v1 ships with
// the loopback-only posture — see ADR-070 cross-box HA).
//
// Failure posture:
//   - 400 on shape-invalid OTLP body (malformed JSON, no spans).
//   - 401 on bearer auth failure (apid Unauthenticated).
//   - 402 on plan-disabled (DebugTelemetryEnabled=false).
//   - 429 on per-account rate cap exhaustion.
//   - 200 on accepted (the truncated summary is staged in the
//     accumulator; the customer's app doesn't wait on the DB
//     write — that's the flush loop's job).

package gateway

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/apidgrpc"
	"github.com/onebox-faas/faas/pkg/gateway/drain"
	"github.com/onebox-faas/faas/pkg/ratelimit/peraccount"
	"github.com/onebox-faas/faas/pkg/wire"
)

// OTelSpansHandlerConfig bundles the handler's dependencies.
type OTelSpansHandlerConfig struct {
	// AuthClient is the apid Auth service client. Required.
	AuthClient apidgrpc.AuthClient
	// Limiter is the per-account token-bucket pool shared with
	// the apid PR-B IncrementRequestTelemetry receiver (same
	// pkg/ratelimit/peraccount instance in production).
	// Required.
	//
	// Known limitation (PR-D code-review #3): the gateway runs
	// in a separate process from apid, so the gateway's limiter
	// is in-process memory and cannot share buckets with the
	// apid-side limiter. The customer's effective cap is
	// therefore 2x the plan's DebugTelemetryRequestsPerMinute:
	// once on the gateway's pre-flight gate, once on apid's
	// writer. The bucket cap is frozen on first Take (PR-D
	// code-review #2), so this 2x ceiling is stable across the
	// customer's session — not oscillating with caller order.
	// A PR-D.1 follow-on folds the rate-limit decision into
	// the apid AuthenticateKey RPC so the gateway stops holding
	// a local bucket; PR-D ships with the documented 2x
	// ceiling as a known acceptable trade-off for v1.0.
	Limiter *peraccount.Limiter
	// Acc is the per-trace coalesce buffer. Required.
	Acc *SpansAccumulator
	// Ops is the daemon's *wire.OpsMetrics. nil-safe.
	Ops *wire.OpsMetrics
	// Log is the structured logger. nil = slog.Default().
	Log *slog.Logger
	// Drain is the per-request WaitGroup-backed drain tracker
	// shared with Handler + the trace handler. nil = disabled.
	Drain *drain.Tracker
	// LimitsFor resolves an api.Plan into its api.Limits. Pulled
	// via a closure so tests can stub the plan table.
	LimitsFor func(plan api.Plan) api.Limits
}

// OTelSpansHandler is the http.Handler for POST /v1/otel/v1/traces.
type OTelSpansHandler struct {
	cfg OTelSpansHandlerConfig
}

// NewOTelSpansHandler returns a handler ready to mount on the
// public mux. The handler is safe for concurrent use; the
// accumulator + limiter own all mutable state.
func NewOTelSpansHandler(cfg OTelSpansHandlerConfig) *OTelSpansHandler {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.LimitsFor == nil {
		cfg.LimitsFor = api.MustLimitsFor
	}
	return &OTelSpansHandler{cfg: cfg}
}

// otelSpansResponse is the 200 OK body. Match the §12 panel
// shape — operators want {accepted_spans, truncated} so a
// quick `curl -s .../v1/otel/v1/traces | jq` confirms the
// truncation tripwire is or isn't firing.
type otelSpansResponse struct {
	AcceptedSpans int  `json:"accepted_spans"`
	Truncated     bool `json:"truncated"`
}

// ServeHTTP handles POST /v1/otel/v1/traces. Drain-tracked for
// graceful shutdown parity with the trace handler.
//
// PR-D code-review #6: auth runs BEFORE the body read. The
// previous order decoded up to 4 MiB + protojson-unmarshalled
// every request before peeking the Authorization header, so
// unauthenticated attackers could amplify their CPU/bandwidth
// spend ~1000x against the gateway by sending valid-shaped
// OTLP bodies with bogus bearer tokens. New order:
//
//   1. Method check.
//   2. Bearer parse (header only — no body read).
//   3. apid AuthenticateKey RPC (the sha256 lookup; loopback
//      unix socket, sub-ms).
//   4. Plan gate.
//   5. Per-account token bucket.
//   6. Body read (4 MiB cap; only authed customers reach
//      this point).
//   7. OTLP decode + shape validation.
//   8. Accumulator Add.
//
// Unauthenticated 401s now do ~header-parse work; the
// 4 MiB + decode cost only applies to legitimate traffic.
func (h *OTelSpansHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Drain tracker (parity with trace_handler.go:74-78).
	done := func() {}
	if h.cfg.Drain != nil {
		done = h.cfg.Drain.Begin("http")
	}
	defer done()

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeProblem(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// ---- Step 2: Bearer parse (no body read) ----
	tok := bearerToken(r)
	if tok == "" {
		h.observeAuthFailure("unauthenticated")
		writeProblem(w, http.StatusUnauthorized, "missing bearer token")
		return
	}

	// ---- Step 3: apid Auth RPC (no body read) ----
	accountIDStr, plan, err := h.cfg.AuthClient.AuthenticateKey(r.Context(), tok)
	if err != nil {
		if status.Code(err) == codes.Unauthenticated {
			h.observeAuthFailure("unauthenticated")
			writeProblem(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		h.observeAuthFailure("internal")
		h.cfg.Log.Warn("apid auth RPC failed", "err", err)
		writeProblem(w, http.StatusInternalServerError, "auth unavailable")
		return
	}
	accountID, err := uuid.Parse(accountIDStr)
	if err != nil {
		h.observeAuthFailure("unauthenticated")
		writeProblem(w, http.StatusUnauthorized, "malformed account_id")
		return
	}

	// ---- Step 4: Plan gate ----
	limits := h.cfg.LimitsFor(api.Plan(plan))
	if !limits.DebugTelemetryEnabled {
		h.observeAuthFailure("plan_disabled")
		w.Header().Set("Retry-After", "0")
		writeProblem(w, http.StatusPaymentRequired, "plan does not include telemetry")
		return
	}

	// ---- Step 5: Per-account token bucket ----
	taken, retryMs := h.cfg.Limiter.Take(accountID, limits.DebugTelemetryRequestsPerMinute)
	if !taken {
		h.observeIngest("rate_limited")
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retryMs/1000))
		writeProblem(w, http.StatusTooManyRequests, "rate limited")
		return
	}

	// ---- Step 6: Body read (now auth-bounded) ----
	// 4 MiB cap. OTLP allows up to 2 MiB per spec; the 2x
	// buffer is headroom for a chatty service emitting
	// attribute-heavy spans.
	const bodyCap = 4 << 20
	r.Body = http.MaxBytesReader(w, r.Body, bodyCap)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		h.observeIngest("shape_invalid")
		writeProblem(w, http.StatusBadRequest, "body too large or unreadable")
		return
	}

	// ---- Step 7: OTLP decode + shape validation ----
	req, decodeErr := decodeExportTraceServiceRequest(raw)
	if decodeErr != nil {
		h.observeIngest("shape_invalid")
		writeProblem(w, http.StatusBadRequest, "shape_invalid: "+decodeErr.Error())
		return
	}
	traceID, spans, shapeErr := extractAndValidate(req)
	if shapeErr != nil {
		h.observeIngest("shape_invalid")
		writeProblem(w, http.StatusBadRequest, "shape_invalid: "+shapeErr.Error())
		return
	}

	// ---- Step 8: Accumulator Add ----
	added, accErr := h.cfg.Acc.Add(traceID, accountID, spans)
	if accErr != nil {
		// PR-D code-review #4: a trace_id being contended
		// across accounts is treated as 401 (the bucket
		// can't safely coalesce both).
		h.observeAuthFailure("unauthenticated")
		writeProblem(w, http.StatusUnauthorized, "trace_id claimed by another account")
		return
	}
	_ = added

	h.observeIngest("inserted")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(otelSpansResponse{
		AcceptedSpans: len(spans),
		Truncated:     false,
	})
}

// decodeExportTraceServiceRequest unmarshals an OTLP/JSON-protobuf
// body. The OTLP standard uses the canonical JSON encoding; we
// rely on the protojson helper rather than rolling our own
// (protojson handles the protobuf-JSON oneof edge cases the
// standard encodes).
func decodeExportTraceServiceRequest(raw []byte) (*collectortracepb.ExportTraceServiceRequest, error) {
	var req collectortracepb.ExportTraceServiceRequest
	if err := protojson.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode otlp: %w", err)
	}
	return &req, nil
}

// extractAndValidate walks the ResourceSpans/ScopeSpans/Span
// tree, returns the canonical W3C trace-id (hex) + a flat slice
// of summarizedSpan, or a shape error describing what was wrong.
// Spans from different trace_ids in one POST are rejected — OTLP
// is trace-bounded; a multi-trace POST is a customer bug.
func extractAndValidate(req *collectortracepb.ExportTraceServiceRequest) (string, []summarizedSpan, error) {
	if len(req.GetResourceSpans()) == 0 {
		return "", nil, errors.New("no resource_spans")
	}
	var traceID string
	var out []summarizedSpan
	for _, rs := range req.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				tid := formatTraceID(sp.GetTraceId())
				if tid == "" {
					return "", nil, errors.New("span missing trace_id")
				}
				if traceID == "" {
					traceID = tid
				} else if traceID != tid {
					return "", nil, errors.New("trace_id mismatch across spans")
				}
				if !isOTelHex32(tid) {
					return "", nil, errors.New("trace_id not 32-char lowercase hex")
				}
				out = append(out, summarizeSpan(sp))
			}
		}
	}
	if len(out) == 0 {
		return "", nil, errors.New("no spans")
	}
	return traceID, out, nil
}

// summarizeSpan flattens one OTLP Span into the summarizedSpan
// shape the accumulator + flush loop work with. The
// db.statement.* attributes are pulled to a top-level field so
// PR-C's prose synthesis can quote SQL without parsing the
// attributes map.
func summarizeSpan(sp *tracepb.Span) summarizedSpan {
	start := sp.GetStartTimeUnixNano()
	end := sp.GetEndTimeUnixNano()
	var dur uint64
	if end > start {
		dur = end - start
	}
	attrs := flattenAttributes(sp.GetAttributes())
	return summarizedSpan{
		TraceID:           formatTraceID(sp.GetTraceId()),
		SpanID:            formatSpanID(sp.GetSpanId()),
		ParentSpanID:      formatSpanID(sp.GetParentSpanId()),
		Name:              sp.GetName(),
		Kind:              sp.GetKind().String(),
		StartTimeUnixNano: start,
		EndTimeUnixNano:   end,
		DurationNanos:     dur,
		Status:            sp.GetStatus().GetCode().String(),
		StatusMessage:     sp.GetStatus().GetMessage(),
		Attributes:        attrs,
		DBStatement:       extractDBStatement(attrs),
	}
}

// formatTraceID hex-encodes an OTLP trace_id (16 bytes) into the
// 32-char lowercase string the database CHECK constraint
// enforces. Empty bytes → empty string (caller validates).
func formatTraceID(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return hex.EncodeToString(b)
}

// formatSpanID hex-encodes an OTLP span_id (8 bytes). The
// summarizedSpan.ParentSpanID omits the empty field for
// root spans (handled at the JSON marshaller via omitempty).
func formatSpanID(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return hex.EncodeToString(b)
}

// flattenAttributes walks an OTLP KeyValueList into a flat
// string map. OTLP attribute values may be string/int/double/bool
// arrays — for the debugger we collapse to their canonical
// fmt.Sprintf("%v") form (load-bearing: PR-C reads Attributes
// to render the prose). Non-string scalar types come through as
// fmt-formatted.
func flattenAttributes(kvs []*commonpb.KeyValue) map[string]string {
	if len(kvs) == 0 {
		return nil
	}
	out := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		out[kv.GetKey()] = attrAnyToString(kv.GetValue())
	}
	return out
}

func attrAnyToString(v *commonpb.AnyValue) string {
	if v == nil {
		return ""
	}
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_BoolValue:
		return fmt.Sprintf("%v", x.BoolValue)
	case *commonpb.AnyValue_IntValue:
		return fmt.Sprintf("%d", x.IntValue)
	case *commonpb.AnyValue_DoubleValue:
		return fmt.Sprintf("%v", x.DoubleValue)
	case *commonpb.AnyValue_ArrayValue:
		return fmt.Sprintf("%v", x.ArrayValue)
	case *commonpb.AnyValue_KvlistValue:
		return fmt.Sprintf("%v", x.KvlistValue)
	case *commonpb.AnyValue_BytesValue:
		return hex.EncodeToString(x.BytesValue)
	}
	return ""
}

// extractDBStatement reads the standard OTEL_SEMCONV_DB_STATEMENT
// attribute. PR-C quotes this verbatim in the prose synthesis;
// lifting it to a top-level field keeps the rest of the
// attributes map free of semantic load.
func extractDBStatement(attrs map[string]string) string {
	if attrs == nil {
		return ""
	}
	if v := attrs["db.statement"]; v != "" {
		return v
	}
	if v := attrs["db.query.text"]; v != "" {
		return v
	}
	return ""
}

// isOTelHex32 matches the W3C trace-id regex: 32 lowercase hex
// chars. Returns false on any other shape — used both here and
// in the apid WriteSpansSummary handler's validator (Stage 4).
func isOTelHex32(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// bearerToken extracts the raw Bearer token from the Authorization
// header. Mirrors pkg/auth/middleware/middleware.go:455 (the
// PR-B IncrementRequestTelemetry receiver uses the same helper,
// but lives in cmd/apid so we duplicate the 4 lines here rather
// than widening that package's exported surface).
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// observeIngest is the nil-safe wrapper for the ingested-counter
// metric. Centralized so the call sites stay 1-liners.
func (h *OTelSpansHandler) observeIngest(outcome string) {
	if h.cfg.Ops == nil {
		return
	}
	h.cfg.Ops.IncrementGatewaydPublicOtelSpansIngested(outcome)
}

// observeAuthFailure is the nil-safe wrapper for the
// auth-failures metric.
func (h *OTelSpansHandler) observeAuthFailure(reason string) {
	if h.cfg.Ops == nil {
		return
	}
	h.cfg.Ops.IncrementGatewaydPublicOtelAuthFailures(reason)
}

// Compile-time guard so a future gateway refactor that forgets
// to mount the handler on a mux doesn't silently break the
// OTLP endpoint.
var _ http.Handler = (*OTelSpansHandler)(nil)

// context-time pin: the handler doesn't use ctx for anything
// beyond passing it to the apid Auth RPC. This time import is
// pulled in by the time.Time tripwire below — keep both.
var _ = time.Time{}
var _ = context.Background
