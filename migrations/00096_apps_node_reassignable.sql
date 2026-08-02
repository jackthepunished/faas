-- +goose Up
-- +goose StatementBegin

-- filename: 00096_apps_node_reassignable.sql
--
-- Tier A4 — partial composite index used by
-- Store.ListOrphanedApps (ADR-064, follow-up to ADR-062).
--
-- The rebalancer's hot query (pkg/sched/rebalancer.go,
-- Store.ListOrphanedApps) joins apps to compute_nodes and
-- filters
--
--   apps.node_id IS NOT NULL
--     AND apps.status IN ('active', 'evicted_cold')
--     AND NOT EXISTS (compute_nodes WHERE id = apps.node_id
--                                       AND active = true)
--     AND (apps.reassigned_at IS NULL
--          OR apps.reassigned_at < now() - interval '<cooldown>s')
--
-- The first three predicates are the cold-start sweep /
-- live-watcher input set: "apps that need an owner and are
-- eligible for reassignment right now". A composite index
-- over (node_id, status) with a partial predicate WHERE
-- node_id IS NOT NULL AND status IN ('active', 'evicted_cold')
-- keeps the index narrow (≤ the non-deleted app fleet,
-- never the full apps table) while supporting the JOIN and
-- the status filter without a separate scan. The
-- (`active` | `evicted_cold`) set is the apps.status CHECK
-- minus `deleted` — soft-deleted apps are filtered out at
-- the SQL level, so the partial index covers every live
-- app row.
--
-- The reassigned_at filter is NOT indexed here — the
-- partial index from migration 00095
-- (apps_reassigned_at_idx WHERE reassigned_at IS NOT NULL)
-- already covers that, and the rebalancer applies the
-- cooldown predicate after the partial index narrows the
-- candidate set. A query planner on a fleet of ≤ 50,000
-- apps will use this index for the leading WHERE clause and
-- the 00095 index for the trailing cooldown filter.
--
-- Replay-safety: CREATE INDEX IF NOT EXISTS paired with
-- DROP INDEX IF EXISTS in the down block (PR #377 /
-- ADR-041 contract).

create index if not exists apps_node_id_status_partial_idx
  on apps (node_id, status)
  where node_id is not null and status in ('active', 'evicted_cold');

-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin

drop index if exists apps_node_id_status_partial_idx;

-- +goose StatementEnd