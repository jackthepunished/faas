-- +goose Up
-- +goose StatementBegin

-- filename: 00073_snapshots_live_idx.sql
--
-- Partial index supporting pkg/state/pgstore.go::LatestSnapshotBytes
-- (PR #428 review blocker #3). The previous query scanned every
-- non-stale snapshot row per app per cron tick because the only
-- index on snapshots was on (deployment_id, created_at) — which
-- helped the join but did not bound the inner lookup. With the
-- storage rollup cron ticking hourly across every paying account,
-- this scaled as O(apps × snapshots_per_app).
--
-- A partial index on (deployment_id) WHERE stale = false collapses
-- the inner scan to the live rows. Combined with the deployment
-- filter `d.status = 'live'` (added in pgstore.go in this PR), the
-- LatestSnapshotBytes query becomes an Index Scan on the small
-- set of currently-billable rows.
--
-- Non-CONCURRENTLY for the same reason as
-- 00069_metering_ops_surfaces.sql::usage_minutes_account_minute_idx:
-- the migration runner (pkg/db.MigrateUp) wraps every .sql file in
-- a transaction, and CONCURRENTLY cannot run inside a Tx. snapshots
-- is read-mostly (the writer is the imaged + builderd pipelines, both
-- of which throttle during a brief migration boot pause), so a
-- non-CONCURRENTLY build locks writes for ~1 s on the EX44 fleet —
-- acceptable.
--
-- Slot history: 73 (renumbered from 71 because PR #429 holds 71
-- via the _reserve_slot carve-out plus a real 00072 migration;
-- collision was caught by the ci.yml migration slot gate and
-- fixed by renumbering to the next free slot).

CREATE INDEX IF NOT EXISTS snapshots_live_idx
    ON snapshots (deployment_id)
    WHERE stale = false;

COMMENT ON INDEX snapshots_live_idx IS
    'Supports pkg/state/pgstore.go::LatestSnapshotBytes inner scan — bounds to non-stale rows under live deployments. ADR-049 §B.3 + PR #428 review blocker #3.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS snapshots_live_idx;
-- +goose StatementEnd
