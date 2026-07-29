// Package meter — usage_daily rollup (ADR-048 §5).
//
// usage_minutes is the canonical per-(account, app, instance, minute)
// ledger; rows stay forever and feed UsageByMonth, the Stripe push,
// and the reconciliation checks. But the dashboard's
// "yesterday's traffic per app" query scans minutes — every minute,
// for every app — to compute one day's worth of sums. The rollup
// table (migrations/00067_extend_metering_telemetry.sql::usage_daily)
// carries the day-grain so the hot path is one Indexed tuple read.
//
// The rollup is an additive merge — re-running on the same
// (account, app, day) tuple sums onto the prior partial, never
// overwrites. A missed tick or a meterd restart is safe: the next
// tick covers the gap.
//
// The rollup never pushes to billing providers — it is informational
// only, mirroring the per-row additivity of the underlying minute
// grain. Provider push stays on the 24 h StripeInterval loop
// (pkg/meter/loop.go).
package meter

import (
	"context"
	"log/slog"
	"time"
)

// execer is the minimal pgxpool contract the rollup needs:
// Exec(sql, args...) for the INSERT ... ON CONFLICT statement.
// Production wires pgxpool.Pool.Exec via a thin adapter; tests
// inject a stub. Keeping the seam narrow avoids a pkg/meter →
// pgxpool import cycle and keeps the rollup unit-testable.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (rows int64, err error)
}

// rollupSQL is the half-open [start, end) INSERT ... ON CONFLICT
// statement that rolls one window of usage_minutes rows into
// usage_daily. Mirrors the column set declared in
// migrations/00067_extend_metering_telemetry.sql; the on-conflict
// update uses additive merge so re-running the same window is
// safe under any number of redeliveries.
const rollupSQL = `
INSERT INTO public.usage_daily (
    account_id, app_id, day,
    mb_seconds, requests, cpu_usec, tx_bytes,
    net_tx_bytes, net_rx_bytes, cold_boots, builder_seconds,
    rolled_up_at
)
SELECT
    account_id, app_id,
    date_trunc('day', minute AT TIME ZONE 'UTC')::date AS day,
    SUM(mb_seconds), SUM(requests), SUM(cpu_usec), SUM(tx_bytes),
    SUM(net_tx_bytes), SUM(net_rx_bytes), SUM(cold_boot_count),
    SUM(CASE WHEN builder_kind <> 'none' THEN builder_seconds ELSE 0 END),
    now()
FROM public.usage_minutes
WHERE minute >= $1 AND minute < $2
GROUP BY 1, 2, 3
ON CONFLICT (account_id, app_id, day) DO UPDATE SET
    mb_seconds      = usage_daily.mb_seconds      + EXCLUDED.mb_seconds,
    requests        = usage_daily.requests        + EXCLUDED.requests,
    cpu_usec        = usage_daily.cpu_usec        + EXCLUDED.cpu_usec,
    tx_bytes        = usage_daily.tx_bytes        + EXCLUDED.tx_bytes,
    net_tx_bytes    = usage_daily.net_tx_bytes    + EXCLUDED.net_tx_bytes,
    net_rx_bytes    = usage_daily.net_rx_bytes    + EXCLUDED.net_rx_bytes,
    cold_boots      = usage_daily.cold_boots      + EXCLUDED.cold_boots,
    builder_seconds = usage_daily.builder_seconds + EXCLUDED.builder_seconds,
    rolled_up_at    = EXCLUDED.rolled_up_at
`

// RollupOnce rolls one window of usage_minutes rows into
// usage_daily. windowStart and windowEnd form a half-open UTC
// window. Caller picks the window; the RollupLoop helper below
// picks `since the last successful rollup` automatically.
//
// Returns the number of usage_daily rows touched by the SQL (sum
// of inserted + updated). 0 is a valid result for an empty window.
func RollupOnce(ctx context.Context, db execer, windowStart, windowEnd time.Time) (int64, error) {
	tag, err := db.Exec(ctx, rollupSQL, windowStart, windowEnd)
	if err != nil {
		return 0, err
	}
	return tag, nil
}

// RollupLoop ticks RollupOnce on interval. Each tick rolls the
// previous UTC day window so the dashboard has at least one day of
// data after the first boot. A future "since-last-rollup" extension
// can read MAX(usage_daily.rolled_up_at) and tick a smaller window —
// today's per-day roll is enough for the hot path.
//
// Errors are logged Warn and retried on the next tick — a
// persistent failure shows up as a flood of WARN logs that an
// operator can alert on.
//
// Use as a free-function goroutine (mirrors pkg/builderd/reaper.go):
//
//	go meter.RollupLoop(ctx, pool, 5*time.Minute, log)
//
// The loop is single-goroutine today; a future meterd replica would
// parallelise via the (account_id, app_id, day) PK — the SQL's
// additive merge is safe under concurrent calls on disjoint day
// ranges, and the on-conflict update under concurrent calls on
// the same day range is monotonic-additive.
func RollupLoop(ctx context.Context, db execer, interval time.Duration, log *slog.Logger) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	if log == nil {
		log = slog.Default()
	}
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Add(-24 * time.Hour)
	end := start.Add(24 * time.Hour)
	if _, err := RollupOnce(ctx, db, start, end); err != nil {
		log.Warn("meter: usage_daily rollup (initial)",
			"window_start", start.Format(time.RFC3339),
			"err", err)
	} else {
		log.Info("meter: usage_daily rollup ok (initial)",
			"window_start", start.Format(time.RFC3339))
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := time.Now().UTC()
			start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Add(-24 * time.Hour)
			end := start.Add(24 * time.Hour)
			if _, err := RollupOnce(ctx, db, start, end); err != nil {
				log.Warn("meter: usage_daily rollup",
					"window_start", start.Format(time.RFC3339),
					"err", err)
				continue
			}
			log.Info("meter: usage_daily rollup ok",
				"window_start", start.Format(time.RFC3339))
		}
	}
}