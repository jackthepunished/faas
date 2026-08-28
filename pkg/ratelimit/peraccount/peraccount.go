// Package peraccount is a per-account token-bucket pool for
// rate-limiting telemetry writes (ADR-127 PR-B and PR-D).
//
// History: extracted from cmd/apid/grpc_server_request_telemetry.go
// in ADR-127 PR-D so both the apid IncrementRequestTelemetry gRPC
// receiver (PR-B) and the gatewayd-public OTel spans writer
// (PR-D) share the same per-account bucket semantics.
//
// Behavior: refill rate is `bucketCap / 60` tokens per second;
// bucket capacity is the full `bucketCap` (one minute's worth of
// tokens, so a customer can burst one full minute before
// back-pressure kicks in). A bucketCap of 0 disables the limiter —
// every Take returns rate-limited, matching the "plan doesn't
// include telemetry" code path.
//
// Concurrency: the inner maps are guarded by a single mutex. The
// per-bucket ops are O(1) (refill calc + token decrement). For
// thousands of accounts the contention is fine — the receivers'
// hot path runs once per row, not per goroutine.
//
// Plan caching: the limiter caches the resolved api.Limits per
// account so a sustained-overflow customer doesn't pay an
// AccountByID round-trip per row. The cache TTL is 60s — a plan
// upgrade takes effect within a minute (well under the customer's
// perception).
package peraccount

import (
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/onebox-faas/faas/pkg/api"
)

// Limiter is the per-account token-bucket pool. The zero value is
// not usable; call NewLimiter. The receiver methods are nil-safe
// for the metrics hook (SetMetricsObserver) but the Take / Cache
// / Cached paths require a real Limiter.
type Limiter struct {
	mu      sync.Mutex
	bucket  map[uuid.UUID]*accountBucket
	limits  map[uuid.UUID]api.Limits // plan-derived caps, TTL 60s
	cacheAt map[uuid.UUID]time.Time
	now     func() time.Time // clock injection seam for tests
}

// accountBucket is the per-account token-bucket state.
//
// cap is the immutable bucket capacity — set on the first Take
// call for this account and frozen thereafter. Freezing the cap
// matters because the limiter is now shared across call sites
// (PR-B IncrementRequestTelemetry passes Hobby/Pro/Scale caps
// derived from per-account limits; PR-D WriteSpansSummary falls
// back to PlanScale on cache miss). If the cap were recomputed
// from each caller's argument, two callers passing different
// caps would oscillate the refill rate on the same bucket —
// the effective capacity would be whichever caller ran last,
// defeating the plan ceiling. With cap frozen on first Take,
// the first caller's plan wins and every subsequent caller
// matches it.
type accountBucket struct {
	tokens     float64
	lastRefill time.Time
	cap        int
	// capSet guards against late-bound cap changes: a second
	// caller passing a different cap compared to the frozen
	// value is rejected via the metric / log so operator can
	// see the wiring bug. We don't silently overwrite — a
	// silent overwrite is how the original bug surfaced.
	capSet bool
}

// NewLimiter wires an empty limiter using time.Now() as the clock.
// Tests can swap the clock via SetClock before calling Take.
func NewLimiter() *Limiter {
	return &Limiter{
		bucket:  make(map[uuid.UUID]*accountBucket),
		limits:  make(map[uuid.UUID]api.Limits),
		cacheAt: make(map[uuid.UUID]time.Time),
		now:     time.Now,
	}
}

// SetClock swaps the clock used by Take for cache-refill
// calculations. Pass a function returning a deterministic
// monotonic time in tests. Restoring production behavior is a
// SetClock(time.Now) call.
func (r *Limiter) SetClock(now func() time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.now = now
}

// Take returns (true, 0) when a token was taken successfully, or
// (false, retryAfterMs) when the bucket was empty. bucketCap
// (DebugTelemetryRequestsPerMinute) caps the bucket; a value of
// 0 disables the limiter (every call is rate-limited — the
// customer's plan doesn't include telemetry).
func (r *Limiter) Take(accountID uuid.UUID, bucketCap int) (bool, int64) {
	if bucketCap <= 0 {
		return false, 60_000 // 60s — match the standard rate-limit hint
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	b, ok := r.bucket[accountID]
	if !ok {
		// First call for this account: freeze the cap. Every
		// subsequent Take / refill must use this cap.
		b = &accountBucket{
			tokens:     float64(bucketCap),
			lastRefill: now,
			cap:        bucketCap,
			capSet:     true,
		}
		r.bucket[accountID] = b
	} else {
		if b.capSet && b.cap != bucketCap {
			// Cap-drift wiring bug. PR-B's per-account
			// DebugTelemetryRequestsPerMinute and PR-D's
			// fallback must agree — they don't share a
			// cache on the first call, so a writer that
			// runs before the recorder sees a different
			// cap than the recorder later sees. The frozen
			// cap wins; the caller's arg is dropped.
			//
			// In practice this never fires once the shared
			// limiter (PR-D fix #3) is wired and the cache
			// is pre-warmed. The branch is the safety net.
			slog.Default().Warn("peraccount: caller-supplied cap drifted from frozen bucket cap; using frozen value",
				"account_id", accountID,
				"frozen_cap", b.cap,
				"caller_cap", bucketCap)
		}
		// Refill: tokens accrue at b.cap / 60 per second.
		// Always use the FROZEN cap, not the caller's arg.
		elapsed := now.Sub(b.lastRefill).Seconds()
		if elapsed > 0 {
			b.tokens += elapsed * float64(b.cap) / 60.0
			if b.tokens > float64(b.cap) {
				b.tokens = float64(b.cap)
			}
			b.lastRefill = now
		}
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	// Empty bucket. retry_after_ms = time until the next token
	// accrues (one minute-token is b.cap / 60 per second → 1
	// token per (60 / b.cap) seconds → 1000 * (60 / b.cap)
	// milliseconds).
	retryMs := int64(60_000 / b.cap)
	if retryMs < 1 {
		retryMs = 1
	}
	return false, retryMs
}

// CacheLimits stores the resolved per-account caps. Called once
// per AccountByID round-trip (not per row).
func (r *Limiter) CacheLimits(accountID uuid.UUID, limits api.Limits) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.limits[accountID] = limits
	r.cacheAt[accountID] = r.now()
}

// CachedLimits returns the cached limits + true if fresh, or
// zero limits + false if the cache entry is older than 60s or
// absent. Caller is responsible for the AccountByID round-trip
// on cache miss.
func (r *Limiter) CachedLimits(accountID uuid.UUID) (api.Limits, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cachedAt, ok := r.cacheAt[accountID]
	if !ok || r.now().Sub(cachedAt) > 60*time.Second {
		return api.Limits{}, false
	}
	return r.limits[accountID], true
}
