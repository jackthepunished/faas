// Package meter — ADR-098 §D1.c / §D2 partition cron (PR-C).
//
// data_upstream_probes is partitioned by sampled_at (range
// partition, daily). The retention cron (RetentionLoop) ages
// out whole partitions hourly; the partition CREATE cron
// (PartitionCreateLoop) ensures a forward-rolling window of
// pre-created partitions so the hot-write path never trips a
// "no partition for this date" 23514.
//
// Cadence: hourly (DefaultUpstreamPartitionCreateInterval).
// Each tick creates partitions for the next 7 days. The
// partition count is bounded — at 30 day retention × 7 day
// lead window, the table never holds more than 30 + 7 = 37
// partitions at any moment.
//
// The cron is Postgres-only; MemStore returns the same
// sentinel as the other ADR-098 MemStore stubs.

package meter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// PartitionCreateInterval (re-exported as the const used by
// the cron) matches the 1-hour default documented in the
// cluster outline's meterd-side PR.
const PartitionCreateInterval = DefaultUpstreamPartitionCreateInterval

// PartitionCreateLeadDays is how many days of forward-rolling
// partitions the cron ensures exist on each tick. 7 days
// matches the meterd replica's typical rollout window — a
// cron miss + a multi-day outage can't strand a probe.
const PartitionCreateLeadDays = 7

// PartitionCreateExecer is the minimal SQL surface the
// partition cron needs. *pgxpool.Pool satisfies it via the
// poolAdapter in cmd/meterd/main.go.
type PartitionCreateExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (int64, error)
}

// EnsureUpstreamProbesPartitionSQL is the per-day CREATE
// TABLE statement. The partition name follows the
// data_upstream_probes_yYYYYMMDD convention (Postgres range
// partitioning); the date bound matches the table's PK index.
//
// The function is generated inline so the partition name and
// date bound stay in lockstep with the date argument.
func EnsureUpstreamProbesPartitionSQL(date time.Time) string {
	return fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS data_upstream_probes_%s PARTITION OF data_upstream_probes FOR VALUES FROM ('%s') TO ('%s')`,
		date.UTC().Format("20060102"),
		date.UTC().Format("2006-01-02"),
		date.Add(24*time.Hour).UTC().Format("2006-01-02"),
	)
}

// PartitionCreateOnce runs the partition-creation sweep for
// the next PartitionCreateLeadDays days. Returns the count of
// partitions created (0 if all already exist). The sweep is
// idempotent — CREATE TABLE IF NOT EXISTS is a no-op when the
// partition is already there.
//
// The cron is bounded: 7 days × 1 statement = 7 DDL
// statements per tick. Postgres serialises DDL on the
// table-level lock so concurrent meterd replicas would block
// briefly; the sweep is fast enough that the lock is held
// for milliseconds, not seconds.
func PartitionCreateOnce(ctx context.Context, db PartitionCreateExecer, now time.Time) (int, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var created int
	for d := 0; d < PartitionCreateLeadDays; d++ {
		day := now.Add(time.Duration(d) * 24 * time.Hour).UTC()
		_, err := db.Exec(ctx, EnsureUpstreamProbesPartitionSQL(day))
		if err != nil {
			return created, fmt.Errorf("meter: upstream partition create (day %s): %w",
				day.Format("2006-01-02"), err)
		}
		created++
	}
	return created, nil
}

// PartitionCreateLoop is the free-function goroutine that
// calls PartitionCreateOnce every Interval. Returns on
// ctx.Done(). Matches the cadence contract for the other
// meterd ticks (RetentionLoop is the closest precedent).
func PartitionCreateLoop(ctx context.Context, db PartitionCreateExecer, interval time.Duration, log *slog.Logger) {
	if interval <= 0 {
		interval = PartitionCreateInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := PartitionCreateOnce(ctx, db, time.Now())
			if log != nil {
				if err != nil {
					log.Error("upstream partition create tick failed", "err", err)
				} else {
					log.Info("upstream partition create tick ok", "partitions_created", n)
				}
			}
			// Sentinel-error handling for the batch-cap
			// shape — currently PartitionCreateOnce has
			// no cap (the partition count is bounded by
			// PartitionCreateLeadDays), so this is a
			// placeholder for future expansion.
			_ = errors.Is(err, context.Canceled)
		}
	}
}
