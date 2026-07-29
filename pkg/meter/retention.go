// Package meter — retention cron (ADR-049 §B.4 + §B.5).
//
// pkg/meter/retention.go enforces the financial-model 13-month
// retention window on public.usage_minutes. Every
// FAAS_RETENTION_INTERVAL (default 1 day, aligned with the §11
// Sunday 04:00 UTC reboot window) the cron DELETEs rows older
// than 13 months. The migration 00069 partial index
// `usage_minutes_minute_idx` keeps the DELETE cheap regardless
// of usage_minutes cardinality.
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

// retentionExecer is the minimal SQL surface the retention cron
// needs. *pgxpool.Pool satisfies it via the poolAdapter in
// cmd/meterd/main.go.
type retentionExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (int64, error)
}

// retentionSQL is the DELETE statement run by RetentionOnce.
// The 13-month cutoff is the financial-model "billing disputes
// window" (§1). Migration 00069 added the partial index on
// (minute) so the scan is O(cutoff_rows), not O(table_size).
const retentionSQL = `DELETE FROM public.usage_minutes
                      WHERE minute < (now() - interval '13 months')`

// RetentionOnce runs one DELETE pass. Returns the number of
// rows deleted. Safe to call concurrently — pgx serialises the
// DELETE at the row level and the WHERE predicate is stable
// across overlapping runs (a second run on the same day finds
// no rows to delete).
func RetentionOnce(ctx context.Context, db retentionExecer) (int64, error) {
	tag, err := db.Exec(ctx, retentionSQL)
	if err != nil {
		return 0, fmt.Errorf("retention delete: %w", err)
	}
	return tag, nil
}

// RetentionLoop is the free-function goroutine that calls
// RetentionOnce every Interval. Returns on ctx.Done(). Matches
// the cadence contract for the other meterd ticks.
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
			if err != nil {
				if log != nil {
					log.Error("retention tick failed", "err", err)
				}
				continue
			}
			if log != nil {
				log.Info("retention tick ok", "rows_deleted", n)
			}
		}
	}
}
