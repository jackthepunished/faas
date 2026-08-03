-- filename: 00132_instances_app_deployment_idx.sql
-- +goose Up
-- +goose StatementBegin

-- Issue #557 closure / ADR-072 — partial index backing the
-- per-deployment wake count that the floor reconciler reads every
-- tick (and the reaper reads every reap cycle). The select the
-- trigger issues is:
--
--   select count(*) from instances
--   where app_id = $1 and deployment_id = $2
--     and state in ('RUNNING', 'WAKING', 'COLD_BOOTING')
--
-- Without an index this is a Seq Scan over `instances` for every
-- (app, deployment) pair the trigger walks; with hundreds of apps
-- and per-app deployments that means N² wake-tick I/O. The partial
-- predicate (3 live states out of 5) keeps the index small — the
-- planner's row estimate for live states is bounded by the RAM
-- admission ceiling (§6.2-2 47,600 MB / (128 MB + 8 MB)) ≈ 350 rows
-- at worst, normally two orders of magnitude smaller.
--
-- Shape choice: (app_id, deployment_id) prefix matches the WHERE
-- clause's equality columns. A future per-deployment reaper query
-- (`select ... where deployment_id = $1 order by started_at desc`)
-- can use this same index as a prefix scan; the existing
-- instances_concurrency_app_idx handles the per-app-only path.
--
-- Replay-safe (PR #377 / ADR-041): CREATE INDEX IF NOT EXISTS. A
-- second MigrateUp is a clean no-op.
--
-- Cardinality budget at the one-box scale: ~100 apps × ~10 deploys
-- per app × O(floor) live rows per deploy = ~3,500 index entries —
-- two orders of magnitude below the budget that would force the
-- planner off the index (Postgres falls back to Seq Scan when the
-- estimated cost is below the table-scan cost, which here is
-- the inverse — the index is cheap and the Seq Scan is expensive).

CREATE INDEX IF NOT EXISTS instances_app_deployment_idx
    ON instances (app_id, deployment_id)
    WHERE state IN ('RUNNING', 'WAKING', 'COLD_BOOTING');

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS public.instances_app_deployment_idx;

-- +goose StatementEnd