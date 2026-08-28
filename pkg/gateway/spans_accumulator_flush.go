// Package gateway — spans_accumulator_flush.go (ADR-127 PR-D).
//
// The flush loop drains the per-trace coalesce accumulator
// (Stage 3) every interval (default 30s) and ships each
// (trace_id, summary_json, account_id) triple to apid's
// SpansWriter gRPC service via the writeFn callback.
//
// Outcomes:
//   - "inserted"     → drop the entry (the DB has it).
//   - "rate_limited" → drop the entry + log + apply the
//                      Retry-After hint (the customer's bucket
//                      is exhausted; the entry is non-critical).
//   - "db_error"     → keep the entry, retry at the next tick
//                      (transient Postgres trip).
//   - non-nil gRPC   → keep the entry, retry at the next tick.
//                      Bounded retries (default 3) → drop + log
//                      to prevent unbounded growth.
//
// Failure posture: the loop is best-effort. A misbehaving
// accumulator entry that errors forever is bounded-dropped;
// a transient DB blip retries naturally on the next tick;
// the loop never panics.

package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// FlushLoopConfig bundles the flush loop's dependencies.
type FlushLoopConfig struct {
	// Interval is the tick period. Default 30s when zero.
	Interval time.Duration
	// WriteFn ships one coalesced payload to apid. Returns
	// outcome ∈ {inserted, rate_limited, db_error}, retry_ms,
	// and a terminal error for gRPC failures. Required.
	WriteFn func(ctx context.Context, traceID string, summaryJSON []byte, accountID string) (outcome string, retryAfterMs int64, err error)
	// Log is the structured logger. nil = slog.Default().
	Log *slog.Logger
	// MaxRetries caps the per-entry retry budget before the
	// loop drops the entry. Default 3 when zero.
	MaxRetries int
}

// pendingEntry holds an accumulator entry whose write failed
// and is awaiting the next flush window.
type pendingEntry struct {
	traceID   string
	accountID uuid.UUID
	summary   []summarizedSpan
	retries   int
}

// RunFlushLoop drains the accumulator on every tick until ctx
// is cancelled. Returns nil on clean cancellation.
func (s *SpansAccumulator) RunFlushLoop(ctx context.Context, cfg FlushLoopConfig) error {
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 3
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}

	pending := make(map[string]*pendingEntry)
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	cfg.Log.Info("otel spans flush loop started", "interval", cfg.Interval.String())
	for {
		select {
		case <-ctx.Done():
			cfg.Log.Info("otel spans flush loop stopping", "reason", ctx.Err())
			return nil
		case <-ticker.C:
			s.drainOnce(ctx, cfg, pending)
		}
	}
}

// drainOnce walks every accumulator bucket, JSON-marshals the
// spans, and calls writeFn. Outcomes drive the pending map:
// inserted/rate_limited → drop; db_error/gRPC error → keep
// with retry counter.
func (s *SpansAccumulator) drainOnce(ctx context.Context, cfg FlushLoopConfig, pending map[string]*pendingEntry) {
	// Walk every bucket and capture the snapshot first; we
	// delete the bucket only on success, so a half-written
	// window is still recoverable.
	s.buckets.Range(func(key, value any) bool {
		traceID, _ := key.(string)
		bucket, _ := value.(*spansAccumulator)
		if traceID == "" || bucket == nil {
			return true
		}
		spans, accountID := bucket.snapshot()
		if len(spans) == 0 {
			// Empty after the dedupe; safe to evict.
			s.buckets.Delete(traceID)
			return true
		}
		// Already-pending retries bump their counter instead
		// of starting over.
		if existing, ok := pending[traceID]; ok {
			existing.summary = spans
			existing.accountID = accountID
		} else {
			pending[traceID] = &pendingEntry{
				traceID:   traceID,
				accountID: accountID,
				summary:   spans,
			}
		}
		return true
	})

	// Now iterate the pending map. drainOnce is the single
	// caller; mutating during iteration is safe because we
	// collect the keys first.
	keys := make([]string, 0, len(pending))
	for k := range pending {
		keys = append(keys, k)
	}
	for _, traceID := range keys {
		entry := pending[traceID]
		if entry.retries >= cfg.MaxRetries {
			cfg.Log.Warn("otel spans entry dropped after max retries",
				"trace_id", traceID, "retries", entry.retries)
			delete(pending, traceID)
			s.buckets.Delete(traceID)
			continue
		}
		summaryJSON, err := json.Marshal(entry.summary)
		if err != nil {
			cfg.Log.Error("otel spans marshal failed",
				"trace_id", traceID, "err", err)
			entry.retries++
			continue
		}
		outcome, retryAfterMs, err := cfg.WriteFn(ctx, entry.traceID, summaryJSON, entry.accountID.String())
		switch {
		case err != nil:
			// gRPC-level failure (InvalidArgument, etc).
			// Keep for retry at next tick.
			cfg.Log.Warn("otel spans write RPC failed",
				"trace_id", traceID, "err", err)
			entry.retries++
		case outcome == "inserted":
			delete(pending, traceID)
			s.buckets.Delete(traceID)
		case outcome == "rate_limited":
			cfg.Log.Debug("otel spans write rate-limited",
				"trace_id", traceID, "retry_after_ms", retryAfterMs)
			delete(pending, traceID)
			s.buckets.Delete(traceID)
		case outcome == "db_error":
			entry.retries++
		default:
			// Unknown outcome → drop to avoid wedging the
			// loop on a server-side contract drift.
			cfg.Log.Warn("otel spans write unknown outcome, dropping",
				"trace_id", traceID, "outcome", outcome)
			delete(pending, traceID)
			s.buckets.Delete(traceID)
		}
	}
}
