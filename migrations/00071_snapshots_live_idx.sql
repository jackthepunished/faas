-- +goose Up
-- +goose StatementBegin

-- filename: 00071_snapshots_live_idx.sql
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
-- CONCURRENTLY so the build doesn't lock the table. The migrate
-- runner skips advisory locks for CREATE INDEX CONCURRENTLY because
-- it cannot run inside a transaction — same pattern as
-- 00069_metering_ops_surfaces.sql::usage_minutes_account_minute_idx.
--
-- Slot history: 71 (next free after 00070_snapshot_storage_daily.sql).

CREATE INDEX CONCURRENTLY IF NOT EXISTS snapshots_live_idx
    ON public.snapshots (deployment_id)
    WHERE stale = false;

COMMENT ON INDEX public.snapshots_live_idx IS
    'Supports pkg/state/pgstore.go::LatestSnapshotBytes inner scan — bounds to non-stale rows under live deployments. ADR-049 §B.3 + PR #428 review blocker #3.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS public.snapshots_live_idx;
-- +goose StatementEnd
