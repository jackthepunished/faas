// Package mirror — mirror_invocation_summary rollup + ledger retention.
//
// The mirror goroutine (pkg/gateway/mirror_dispatch.go) writes one
// mirror_invocation_results row per dispatch via
// state.Store.InsertMirrorResult. The ledger is the per-invocation
// audit trail; rows stay for 7 days so operators can diagnose a
// customer's "my mirror isn't firing" complaint with a SQL query.
//
// The dashboard's mirror summary chip and the §12 mirror drift-rate
// alert want hour-grain counts over the rule lifetime without
// scanning the raw table on every read. mirror_invocation_summary
// (migrations/00501_mirror_invocation_summary.sql) is the rollup
// table; this package owns the writer.
//
// Design parallels pkg/meter/rollup.go (usage_daily) and
// pkg/sched/retention.go (instance row sweep):
//
//   - RollupOnce / RollupLoop: additive-merge UPSERT keyed on
//     (rule_id, hour_bucket). Re-running on an already-collected
//     hour ADDS the new ledger rows to the existing count; the
//     raw ledger is append-only so the running sum is the
//     monotonic-correct answer (contrast usage_daily which
//     overwrites because it sums over a full day window).
//   - SweepOldLedgerRows: DELETE mirror_invocation_results rows
//     older than 7d. Best-effort; the rollup already preserved
//     the per-hour counts.
//
// The two halves are decoupled so the retention sweep can run on a
// different cadence from the rollup (e.g. daily sweep vs hourly
// rollup). RollupLoop runs both inside one goroutine so production
// only has to wire one.
package mirror

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// DefaultRollupInterval is the default tick for the rollup +
// retention sweep goroutine. Mirrors pkg/meter's 5-minute cadence
// — small enough that a meterd restart covers a missed tick in
// ~one cycle, large enough that the SQL doesn't dominate the
// connection pool.
const DefaultRollupInterval = 5 * time.Minute

// DefaultLedgerRetention is how long raw mirror_invocation_results
// rows stay before the sweep deletes them. 7 days matches the
// ADR-124 "customer can debug last week's mirror" contract; longer
// than 7d starts to dominate the table (mirror fan-out volume).
const DefaultLedgerRetention = 7 * 24 * time.Hour

// execer is the minimal contract the rollup + sweep need:
// Exec(sql, args...) for the UPSERT + DELETE statements.
// Production wires pgxpool.Pool.Exec via a thin adapter (see
// cmd/meterd/main.go::poolAdapter for the canonical shape);
// tests inject a stub. Same seam as pkg/meter/rollup.go::execer
// — keeps the rollup unit-testable without a Postgres dependency.
//
// The interface signature uses int64 because that's what pgx's
// pooled connections return for the affected-row count. Test
// stubs can return any int64 (typically 0 for INSERT/UPDATE, N
// for DELETE).
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (rows int64, err error)
}

// rollupSQL is the additive-merge UPSERT statement that rolls one
// window of mirror_invocation_results rows into
// mirror_invocation_summary. windowStart and windowEnd form a
// half-open [start, end) window truncated to the hour boundary on
// GROUP BY. On conflict (same rule_id, same hour_bucket) the
// counters ADD the new contribution to the existing value — re-
// running the rollup on a partially-collected hour converges to
// the same final count as a single end-of-hour run.
//
// Status diff / schema diff / body diff / crash counts are
// summed separately so the dashboard chip can render any subset
// of the four flags. cap_at_max_count tracks the per-rule slot
// saturation events specifically — a saturated rule is operationally
// distinct from a 5xx crash and ops alerts on each independently.
const rollupSQL = `
INSERT INTO mirror_invocation_summary (
    rule_id, app_id, hour_bucket,
    total_invocations, status_diff_count, schema_diff_count,
    body_diff_count, crash_count, cap_at_max_count,
    sum_latency_ms, rolled_up_at
)
SELECT
    mirror_rule_id,
    (SELECT app_id FROM mirror_rules WHERE id = mirror_rule_id),
    date_trunc('hour', completed_at) AS hour_bucket,
    COUNT(*),
    COUNT(*) FILTER (WHERE status_diff),
    COUNT(*) FILTER (WHERE schema_diff),
    COUNT(*) FILTER (WHERE body_diff),
    COUNT(*) FILTER (WHERE crashed),
    COUNT(*) FILTER (WHERE cap_at_max),
    COALESCE(SUM(latency_ms), 0),
    now()
FROM mirror_invocation_results
WHERE completed_at >= $1 AND completed_at < $2
GROUP BY 1, 3
ON CONFLICT (rule_id, hour_bucket) DO UPDATE SET
    total_invocations = mirror_invocation_summary.total_invocations + EXCLUDED.total_invocations,
    status_diff_count = mirror_invocation_summary.status_diff_count + EXCLUDED.status_diff_count,
    schema_diff_count = mirror_invocation_summary.schema_diff_count + EXCLUDED.schema_diff_count,
    body_diff_count   = mirror_invocation_summary.body_diff_count   + EXCLUDED.body_diff_count,
    crash_count       = mirror_invocation_summary.crash_count       + EXCLUDED.crash_count,
    cap_at_max_count  = mirror_invocation_summary.cap_at_max_count  + EXCLUDED.cap_at_max_count,
    sum_latency_ms    = mirror_invocation_summary.sum_latency_ms    + EXCLUDED.sum_latency_ms,
    rolled_up_at      = now()
`

// sweepSQL deletes mirror_invocation_results rows older than the
// cutoff. Best-effort: a sweep failure is logged Warn and retried
// on the next tick. The rollup preserved the per-hour counts so
// the loss of raw rows doesn't affect the dashboard chip.
const sweepSQL = `
DELETE FROM mirror_invocation_results
WHERE completed_at < $1
`

// RollupOnce rolls one window of mirror_invocation_results rows
// into mirror_invocation_summary. windowStart and windowEnd form
// a half-open [start, end) UTC window. Returns the number of
// mirror_invocation_summary rows touched by the SQL (inserted +
// updated). 0 is a valid result for an empty window.
func RollupOnce(ctx context.Context, db execer, windowStart, windowEnd time.Time) (int64, error) {
	if !windowEnd.After(windowStart) {
		return 0, fmt.Errorf("mirror: rollup window end %s not after start %s", windowEnd, windowStart)
	}
	tag, err := db.Exec(ctx, rollupSQL, windowStart, windowEnd)
	if err != nil {
		return 0, fmt.Errorf("mirror: rollup window [%s,%s): %w", windowStart, windowEnd, err)
	}
	return tag, nil
}

// SweepOldLedgerRows deletes mirror_invocation_results rows older
// than cutoff. Returns the number of rows deleted. The dashboard
// chip is unaffected because mirror_invocation_summary already
// preserves the per-hour counts.
func SweepOldLedgerRows(ctx context.Context, db execer, cutoff time.Time) (int64, error) {
	tag, err := db.Exec(ctx, sweepSQL, cutoff)
	if err != nil {
		return 0, fmt.Errorf("mirror: sweep rows older than %s: %w", cutoff, err)
	}
	return tag, nil
}

// RollupLoop ticks RollupOnce + SweepOldLedgerRows on interval.
// The initial tick covers the trailing 24h so a freshly-booted
// daemon has a populated summary immediately. Subsequent ticks
// cover the trailing interval (default 5m). After each rollup
// tick, the sweep runs with the DefaultLedgerRetention cutoff so
// the raw ledger doesn't grow unbounded.
//
// Errors are logged Warn and retried on the next tick — a
// persistent failure surfaces as a flood of WARN logs an operator
// can alert on. Mirrors pkg/meter/rollup.go::RollupLoop's
// contract.
//
// Use as a free-function goroutine from cmd/schedd/main.go:
//
//	go mirror.RollupLoop(ctx, pool, mirror.DefaultRollupInterval, log)
func RollupLoop(ctx context.Context, db execer, interval time.Duration, log *slog.Logger) {
	if interval <= 0 {
		interval = DefaultRollupInterval
	}
	if log == nil {
		log = slog.Default()
	}
	// Initial tick: roll the trailing 24h so the dashboard
	// has summary data on first boot. The first run also
	// picks up any rows the previous daemon's lifetime wrote
	// but never rolled (e.g. a meterd process restart that
	// didn't get to its 5-minute tick).
	end := time.Now().UTC()
	start := end.Add(-24 * time.Hour)
	if _, err := RollupOnce(ctx, db, start, end); err != nil {
		log.Warn("mirror: summary rollup (initial)",
			"window_start", start.Format(time.RFC3339),
			"err", err)
	} else if log != nil {
		log.Info("mirror: summary rollup ok (initial)",
			"window_start", start.Format(time.RFC3339))
	}
	// Sweep on the same boot tick so a fresh daemon doesn't
	// accumulate historical ledger rows.
	cutoff := time.Now().UTC().Add(-DefaultLedgerRetention)
	if _, err := SweepOldLedgerRows(ctx, db, cutoff); err != nil {
		log.Warn("mirror: ledger sweep (initial)", "cutoff", cutoff.Format(time.RFC3339), "err", err)
	}

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case tickEnd := <-t.C:
			tickStart := tickEnd.Add(-interval)
			if _, err := RollupOnce(ctx, db, tickStart, tickEnd); err != nil {
				log.Warn("mirror: summary rollup",
					"window_start", tickStart.Format(time.RFC3339),
					"err", err)
			}
			sweepCutoff := tickEnd.Add(-DefaultLedgerRetention)
			if _, err := SweepOldLedgerRows(ctx, db, sweepCutoff); err != nil {
				log.Warn("mirror: ledger sweep",
					"cutoff", sweepCutoff.Format(time.RFC3339),
					"err", err)
			}
		}
	}
}
