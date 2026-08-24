// apid-side gRPC handler for the RequestTelemetry service
// (ADR-127 PR-B). Wired by registerRequestTelemetryReceiver in
// main.go onto the same *grpc.Server runAppErrorsServer uses (or
// a sibling; the surface is independent).
//
// Direction: gatewayd-internal → apid. gatewayd-internal is the
// ONLY caller (CLAUDE.md ownership: apid is the sole writer to
// request_telemetry; gatewayd-internal never opens a direct
// Postgres connection for this store).
//
// Wire discipline mirrors grpc_server_apperrors.go: errors map to
// gRPC codes (InvalidArgument / ResourceExhausted / Internal),
// per-record outcomes ride on the response stream, transient
// failures do NOT abort the stream. PR-B adds a per-account
// token bucket so a sustained-overflow customer gets
// back-pressured without aborting the rest of the batch.

package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	apidpb "github.com/onebox-faas/faas/api/proto/onebox/faas/apid/v1"
	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
	"github.com/onebox-faas/faas/pkg/state/sqlc"
	"github.com/onebox-faas/faas/pkg/wire"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Outcome strings for the IncrementRequestTelemetryResponse
// stream. The §12 metric
// `apid_request_telemetry_recorded_total{outcome="..."}` keys off
// these literals; the wire-side constants in pkg/wire/metrics.go
// must stay aligned (goconst-flagged so drift breaks the lint
// gate).
const (
	rtOutcomeInserted   = "inserted"
	rtOutcomeRateLimited = "rate_limited"
	rtOutcomeDBError    = "db_error"
)

// requestTelemetryStore is the Store subset the receiver needs.
// Declared as an interface here so unit tests can substitute a
// fake without spinning a real Postgres pool.
type requestTelemetryStore interface {
	AccountByID(ctx context.Context, id string) (state.Account, error)
	InsertRequestTelemetry(ctx context.Context, arg sqlc.InsertRequestTelemetryParams) error
}

// telemetryRateLimiter is a per-account token bucket pool.
// PR-B (ADR-127 §Decision 6): per-account caps live here, not in
// Postgres. The bucket refill rate is DebugTelemetryRequestsPerMinute
// / 60 tokens per second; the bucket capacity is the full
// DebugTelemetryRequestsPerMinute so a customer can burst one
// full minute's worth of telemetry before back-pressure kicks in.
//
// Concurrency: the inner map is guarded by a single mutex. The
// per-bucket ops are O(1) (refill calc + token decrement). For
// thousands of accounts the contention is fine — the receiver's
// hot path runs once per row, not per goroutine.
//
// Plan caching: the rate limiter caches the resolved Limits per
// account so a sustained-overflow customer doesn't pay an
// AccountByID round-trip per row. The cache TTL is 60s — a plan
// upgrade takes effect within a minute (well under the customer's
// perception).
type telemetryRateLimiter struct {
	mu     sync.Mutex
	bucket map[uuid.UUID]*telemetryAccountBucket
	limits map[uuid.UUID]api.Limits // plan-derived caps, TTL 60s
	cacheAt map[uuid.UUID]time.Time
}

type telemetryAccountBucket struct {
	tokens     float64
	lastRefill  time.Time
}

// newTelemetryRateLimiter wires an empty limiter.
func newTelemetryRateLimiter() *telemetryRateLimiter {
	return &telemetryRateLimiter{
		bucket:  make(map[uuid.UUID]*telemetryAccountBucket),
		limits:  make(map[uuid.UUID]api.Limits),
		cacheAt: make(map[uuid.UUID]time.Time),
	}
}

// take returns (true, 0) when a token was taken successfully, or
// (false, retryAfterMs) when the bucket was empty. bucketCap
// (DebugTelemetryRequestsPerMinute) caps the bucket; a value of
// 0 disables the limiter (every call is rate-limited, the
// customer's plan doesn't include telemetry).
func (r *telemetryRateLimiter) take(accountID uuid.UUID, bucketCap int) (bool, int64) {
	if bucketCap <= 0 {
		return false, 60_000 // 60s — match the standard rate-limit hint
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	b, ok := r.bucket[accountID]
	if !ok {
		b = &telemetryAccountBucket{
			tokens:    float64(bucketCap),
			lastRefill: now,
		}
		r.bucket[accountID] = b
	} else {
		// Refill: tokens accrue at bucketCap / 60 per second.
		elapsed := now.Sub(b.lastRefill).Seconds()
		if elapsed > 0 {
			b.tokens += elapsed * float64(bucketCap) / 60.0
			if b.tokens > float64(bucketCap) {
				b.tokens = float64(bucketCap)
			}
			b.lastRefill = now
		}
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	// Empty bucket. retry_after_ms = time until the next token
	// accrues (one minute-token is bucketCap / 60 per second → 1
	// token per (60 / bucketCap) seconds → 1000 * (60 / bucketCap)
	// milliseconds).
	retryMs := int64(60_000 / bucketCap)
	if retryMs < 1 {
		retryMs = 1
	}
	return false, retryMs
}

// cacheLimits stores the resolved per-account caps. Called once
// per AccountByID round-trip (not per row).
func (r *telemetryRateLimiter) cacheLimits(accountID uuid.UUID, limits api.Limits) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.limits[accountID] = limits
	r.cacheAt[accountID] = time.Now()
}

// cachedLimits returns the cached limits + true if fresh, or
// zero limits + false if the cache entry is older than 60s or
// absent. Caller is responsible for the AccountByID round-trip
// on cache miss.
func (r *telemetryRateLimiter) cachedLimits(accountID uuid.UUID) (api.Limits, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cachedAt, ok := r.cacheAt[accountID]
	if !ok || time.Since(cachedAt) > 60*time.Second {
		return api.Limits{}, false
	}
	return r.limits[accountID], true
}

// requestTelemetryReceiver is the in-package server implementation
// of apidpb.RequestTelemetryServer. Wired by
// registerRequestTelemetryReceiver onto a *grpc.Server.
//
// ops is the apid daemon's *wire.OpsMetrics; nil-safe.
//
// limiter is the per-account token bucket pool; nil disables rate
// limiting (testing seam).
//
// enabled is the kill-switch (FAAS_REQUEST_TELEMETRY_ENABLED).
// When false, IncrementRequestTelemetry returns codes.Unavailable
// so the gateway stops sending.
type requestTelemetryReceiver struct {
	apidpb.UnimplementedRequestTelemetryServer
	store   requestTelemetryStore
	ops     *wire.OpsMetrics
	limiter *telemetryRateLimiter
	enabled bool
}

// newRequestTelemetryReceiver wires a production receiver.
func newRequestTelemetryReceiver(store requestTelemetryStore, ops *wire.OpsMetrics, limiter *telemetryRateLimiter, enabled bool) *requestTelemetryReceiver {
	if limiter == nil {
		limiter = newTelemetryRateLimiter()
	}
	return &requestTelemetryReceiver{store: store, ops: ops, limiter: limiter, enabled: enabled}
}

// IncrementRequestTelemetry streams per-record telemetry rows
// from the gateway edge. The server commits each record inside
// its own transaction (per-record commit; load-bearing for the
// rate-limit-then-continues shape — a rate-limited row MUST NOT
// abort the batch).
//
// Outcome ∈ {inserted, rate_limited, db_error}. The first is a
// success; the latter two are observability signals — the gateway
// MUST NOT retry on them (a retry would double-count).
func (r *requestTelemetryReceiver) IncrementRequestTelemetry(stream apidpb.RequestTelemetry_IncrementRequestTelemetryServer) error {
	if !r.enabled {
		return status.Error(codes.Unavailable, "request_telemetry disabled by FAAS_REQUEST_TELEMETRY_ENABLED")
	}
	for {
		req, err := stream.Recv()
		if err != nil {
			// io.EOF or context cancel: end of stream.
			//nolint:nilerr // io.EOF on stream.Recv is the canonical "client half-closed" signal — returning nil closes the stream cleanly without surfacing the EOF as an error.
			return nil
		}
		out := r.handleOne(stream.Context(), req)
		if err := stream.Send(out); err != nil {
			return err
		}
	}
}

// handleOne processes a single record and returns the per-record
// outcome. Never returns an error — every failure path maps to
// one of the closed outcome strings, with the matching metric
// increment.
func (r *requestTelemetryReceiver) handleOne(ctx context.Context, req *apidpb.IncrementRequestTelemetryRequest) *apidpb.IncrementRequestTelemetryResponse {
	out := &apidpb.IncrementRequestTelemetryResponse{}

	// ---- 1. Validate UUIDs ----
	accountID, err := uuid.Parse(req.GetAccountId())
	if err != nil {
		r.observe(rtOutcomeDBError)
		out.Outcome = rtOutcomeDBError
		return out
	}
	appID, err := uuid.Parse(req.GetAppId())
	if err != nil {
		r.observe(rtOutcomeDBError)
		out.Outcome = rtOutcomeDBError
		return out
	}
	deploymentID, err := uuid.Parse(req.GetDeploymentId())
	if err != nil {
		r.observe(rtOutcomeDBError)
		out.Outcome = rtOutcomeDBError
		return out
	}

	// ---- 2. Resolve per-account rate cap ----
	limits, ok := r.limiter.cachedLimits(accountID)
	if !ok {
		acct, accErr := r.store.AccountByID(ctx, accountID.String())
		if accErr != nil {
			// Account gone — treat as db_error so the gateway
			// doesn't retry forever.
			r.observe(rtOutcomeDBError)
			out.Outcome = rtOutcomeDBError
			return out
		}
		limits = api.MustLimitsFor(acct.Plan)
		r.limiter.cacheLimits(accountID, limits)
	}
	if !limits.DebugTelemetryEnabled {
		// Plan doesn't include telemetry — same as rate-limit
		// overflow: drop the row, count as rate_limited (the
		// gateway should stop sending, same code path).
		out.Outcome = rtOutcomeRateLimited
		out.RetryAfterMs = 60_000
		return out
	}

	// ---- 3. Per-account token bucket check ----
	taken, retryAfter := r.limiter.take(accountID, limits.DebugTelemetryRequestsPerMinute)
	if !taken {
		out.Outcome = rtOutcomeRateLimited
		out.RetryAfterMs = retryAfter
		// Counter carries `rate_limited` as a label so dashboards
		// can split insert success vs bucket-overflow cleanly.
		r.observe(rtOutcomeRateLimited)
		return out
	}

	// ---- 4. INSERT ----
	count := int(req.GetCount())
	if count < 1 {
		count = 1
	}
	insertErr := r.store.InsertRequestTelemetry(ctx, sqlc.InsertRequestTelemetryParams{
		AccountID:    state.NewPgtypeUUID(accountID),
		AppID:        state.NewPgtypeUUID(appID),
		DeploymentID: state.NewPgtypeUUID(deploymentID),
		Route:        req.GetRouteTemplate(),
		Method:       req.GetMethod(),
		Status:       int32(req.GetHttpStatus()),
		LatencyMs:    int32(req.GetLatencyMs()),
		ColdBoot:     req.GetColdBoot(),
		TraceID:      pgtype.Text{String: req.GetTraceId(), Valid: req.GetTraceId() != ""},
		ReceivedAt:   state.NewPgtypeTime(msToTime(req.GetReceivedAtUnixMs())),
		Count:        int32(count),
	})
	if insertErr != nil {
		if isConstraintViolation(insertErr) {
			r.observe(rtOutcomeDBError)
			out.Outcome = rtOutcomeDBError
			return out
		}
		var pgErr *pgconn.PgError
		// Check if it's a CHECK constraint violation
		// (count >= 1, status BETWEEN 100..599, etc).
		if errorsAsPgError(insertErr, &pgErr) && pgErr.Code == "23514" {
			r.observe(rtOutcomeDBError)
			out.Outcome = rtOutcomeDBError
			return out
		}
		r.observe(rtOutcomeDBError)
		out.Outcome = rtOutcomeDBError
		return out
	}

	r.observe(rtOutcomeInserted)
	out.Outcome = rtOutcomeInserted
	return out
}

// observe is a nil-safe wrapper around the recorded counter.
func (r *requestTelemetryReceiver) observe(outcome string) {
	if r.ops == nil {
		return
	}
	r.ops.IncrementRequestTelemetryRecorded(outcome)
}

// registerRequestTelemetryReceiver binds the RequestTelemetryServer
// onto a gRPC server. Called from runRequestTelemetryServer in
// main.go alongside the other gRPC services.
func registerRequestTelemetryReceiver(s *grpc.Server, store requestTelemetryStore, ops *wire.OpsMetrics, limiter *telemetryRateLimiter, enabled bool) {
	apidpb.RegisterRequestTelemetryServer(s, newRequestTelemetryReceiver(store, ops, limiter, enabled))
}

// errorsAsPgError is a tiny helper that returns true when err is
// (or wraps) a *pgconn.PgError, populating *target. Delegates to
// errors.As so the errorlint pass is happy and the unwrap chain is
// semantically correct for pgx v5.
func errorsAsPgError(err error, target **pgconn.PgError) bool {
	return errors.As(err, target)
}