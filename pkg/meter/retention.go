// Package meter — retention cron (ADR-049 §B.4 + §B.5).
//
// pkg/meter/retention.go enforces the financial-model 13-month
// retention window on public.usage_minutes. Every
// FAAS_RETENTION_INTERVAL (default 1 day, aligned with the §11
// Sunday 04:00 UTC reboot window) the cron DELETEs rows older
// than 13 months. The migration 00069 partial index
// `usage_minutes_minute_idx` keeps the per-batch DELETE cheap
// regardless of usage_minutes cardinality.
//
// The DELETE is BATCHED (PR #428 review blocker #4) —
// an unbounded DELETE on a 5 M+ rows table would either take a
// statement_timeout-sized lock or balloon WAL on the EX44.
// RetentionOnce runs the batched DELETE in a loop with a hard
// iteration cap (`MaxRetentionBatches = 1000`, ~10 M rows / day
// at 10 000 rows/batch) and returns the cumulative row count.
//
// This is the B.4.c shape (DELETE cron, not declarative
// partitioning). Partitioning lands in a follow-up PR after
// weekly DELETE behaviour is measured (vacuum cost on a 5 M
// rows/month table is non-trivial on the EX44).
//
// Synthetic-row recovery (B.5) is a scaffold-only this PR:
// pkg/meter/sampler.go detects a ≥ 2-tick gap and logs a
// warning. The synthetic column + backfill insert are deferred
// to a follow-up PR.
package meter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// DefaultRetentionInterval is the cadence the retention cron
// runs at when the caller passes 0. 1 day matches the §11
// Sunday reboot window — the DELETE runs on a daily schedule
// even though the financial-model retention is 13 months, so a
// single missed tick is harmless.
const DefaultRetentionInterval = 24 * time.Hour

// RetentionBatchSize is the number of rows deleted per
// statement. 10 000 keeps a single DELETE under Postgres's
// default 1000 ms statement_timeout on the EX44's modest
// hardware while still being a meaningful chunk on a 5 M
// rows/month table. Tunable via FAAS_RETENTION_BATCH_SIZE if a
// future migration to partitioning raises the per-batch budget.
const RetentionBatchSize = 10_000

// MaxRetentionBatches caps the loop in RetentionOnce at 1000
// iterations = 10 M rows / call. Real-world retention at the
// 13-month cutoff will never touch 10 M rows in a single day;
// the cap is a safety belt against an accidental `interval '0'`
// typo or a buggy clock that pins the cutoff to "now()" and
// tries to nuke the entire table.
const MaxRetentionBatches = 1000

// ErrRetentionBatchCap is returned when the loop hits
// MaxRetentionBatches. The caller (RetentionLoop) logs and
// retries on the next tick; the next tick will pick up where
// this one left off because the WHERE predicate is unchanged.
var ErrRetentionBatchCap = errors.New("meter: retention DELETE hit batch cap; row count returned, retry next tick")

// retentionExecer is the minimal SQL surface the retention cron
// needs. *pgxpool.Pool satisfies it via the poolAdapter in
// cmd/meterd/main.go.
type retentionExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (int64, error)
}

// retentionBatchSQL is the per-batch DELETE used by
// RetentionOnce. The sub-select on ctid + LIMIT N is the
// standard Postgres "bounded DELETE" pattern: it gives the
// planner an early-stop on the index scan instead of building a
// full visibility map entry set. The `WHERE ctid IN (SELECT
// ctid ...)` form keeps row-level locks short and avoids the
// "huge DELETE inflates the WAL" pathology the review surfaced.
const retentionBatchSQL = `DELETE FROM public.usage_minutes
                          WHERE ctid IN (
                              SELECT ctid FROM public.usage_minutes
                              WHERE minute < (now() - interval '13 months')
                              LIMIT $1
                          )`

// RetentionOnce runs one DELETE pass, bounded by
// RetentionBatchSize per statement and MaxRetentionBatches
// total iterations. Returns the cumulative row count across
// all batches. Safe to call concurrently — pgx serialises the
// DELETE at the row level and the WHERE predicate is stable
// across overlapping runs (a second run on the same day finds
// no rows to delete).
//
// If the cap is hit, returns (rows_deleted, ErrRetentionBatchCap)
// so the caller can decide to log-and-retry or escalate.
func RetentionOnce(ctx context.Context, db retentionExecer) (int64, error) {
	var total int64
	for i := 0; i < MaxRetentionBatches; i++ {
		tag, err := db.Exec(ctx, retentionBatchSQL, RetentionBatchSize)
		if err != nil {
			return total, fmt.Errorf("retention delete (batch %d, deleted so far %d): %w", i, total, err)
		}
		total += tag
		if tag < RetentionBatchSize {
			// Short read — fewer rows matched the predicate than the
			// batch ceiling, so the next batch would also be a no-op.
			return total, nil
		}
	}
	// Hit the cap. Return the cumulative count + sentinel so the
	// loop logs but doesn't panic. The next tick picks up.
	return total, ErrRetentionBatchCap
}

// RetentionLoop is the free-function goroutine that calls
// RetentionOnce every Interval. Returns on ctx.Done(). Matches
// the cadence contract for the other meterd ticks.
//
// ErrRetentionBatchCap is logged at Warn (not Error) and the
// loop continues — the next tick picks up where this one left
// off because the WHERE predicate is unchanged. Only hard DB
// failures (network, FK violation, etc.) bubble up to Error.
func RetentionLoop(ctx context.Context, db retentionExecer, interval time.Duration, log *slog.Logger) {
	if interval <= 0 {
		interval = DefaultRetentionInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := RetentionOnce(ctx, db)
			switch {
			case err == nil:
				if log != nil {
					log.Info("retention tick ok", "rows_deleted", n)
				}
			case errors.Is(err, ErrRetentionBatchCap):
				if log != nil {
					log.Warn("retention tick hit batch cap; will resume next tick", "rows_deleted", n, "err", err)
				}
			default:
				if log != nil {
					log.Error("retention tick failed", "err", err)
				}
			}
		}
	}
}

// DefaultRequestTelemetryRetentionDays is the upper bound across
// plans (Scale=14, ADR-127 / pkg/api/limits.go). Hobby (3) and
// Pro (7) fall under it — over-retention by ≤ 14 days is the
// safe failure mode (PR-B adds per-app plan-aware filtering).
const DefaultRequestTelemetryRetentionDays = 14

// RequestTelemetryRetentionInterval is the cron cadence for the
// request_telemetry sweep. Hourly matches the smaller per-plan
// retention caps (Hobby=3d) without a meaningful load cost —
// the DELETE is bounded and the index makes it cheap.
const RequestTelemetryRetentionInterval = 1 * time.Hour

// retentionRequestTelemetryBatchSQL deletes rows in the bounded
// DELETE pattern (ctid + LIMIT N) — same shape as
// retentionBatchSQL above. Mirrors the partial index
// request_telemetry_app_received_idx for the planner.
const retentionRequestTelemetryBatchSQL = `DELETE FROM public.request_telemetry
                                         WHERE ctid IN (
                                             SELECT ctid FROM public.request_telemetry
                                             WHERE received_at < (now() - $1::interval)
                                             LIMIT $2
                                         )`

// RetentionOnceRequestTelemetry runs one DELETE pass against
// public.request_telemetry, bounded by RetentionBatchSize per
// statement and MaxRetentionBatches total iterations. Returns
// the cumulative row count. Same shape as RetentionOnce —
// additive free function, no shared state with the usage_minutes
// sweep.
//
// The retention window is DefaultRequestTelemetryRetentionDays
// (14d, the Scale cap). PR-A is over-retentive for Hobby/Pro;
// PR-B adds per-app plan-aware filtering via the limits table.
func RetentionOnceRequestTelemetry(ctx context.Context, db retentionExecer) (int64, error) {
	interval := fmt.Sprintf("%d days", DefaultRequestTelemetryRetentionDays)
	var total int64
	for i := 0; i < MaxRetentionBatches; i++ {
		tag, err := db.Exec(ctx, retentionRequestTelemetryBatchSQL, interval, RetentionBatchSize)
		if err != nil {
			return total, fmt.Errorf("request telemetry retention delete (batch %d, deleted so far %d): %w", i, total, err)
		}
		total += tag
		if tag < RetentionBatchSize {
			return total, nil
		}
	}
	return total, ErrRetentionBatchCap
}

// RetentionLoopRequestTelemetry is the free-function goroutine
// that calls RetentionOnceRequestTelemetry every Interval.
// Returns on ctx.Done(). Mirrors RetentionLoop — same log posture
// (cap-hit is Warn, hard DB failure is Error).
func RetentionLoopRequestTelemetry(ctx context.Context, db retentionExecer, interval time.Duration, log *slog.Logger) {
	if interval <= 0 {
		interval = RequestTelemetryRetentionInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := RetentionOnceRequestTelemetry(ctx, db)
			switch {
			case err == nil:
				if log != nil {
					log.Info("request telemetry retention tick ok", "rows_deleted", n)
				}
			case errors.Is(err, ErrRetentionBatchCap):
				if log != nil {
					log.Warn("request telemetry retention tick hit batch cap; will resume next tick", "rows_deleted", n, "err", err)
				}
			default:
				if log != nil {
					log.Error("request telemetry retention tick failed", "err", err)
				}
			}
		}
	}
}
