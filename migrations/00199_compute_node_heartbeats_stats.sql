-- +goose Up
-- +goose StatementBegin
-- filename: 00199_compute_node_heartbeats_stats.sql
--
-- PR #4 (operator-obs §3.7 follow-up, ADR-091 §3.6 amendment) — extends
-- the existing compute_node_heartbeats table with two columns that
-- back per-node CPU and disk pressure on /v1/admin/obs/nodes.
--
-- Why nullable (NOT NULL would have rejected this migration if vmmd
-- had written a row before vmmd was upgraded):
--
--   The existing fleet has heartbeats with cpu_pct_60s = NULL and
--   disk_used_bytes = NULL (the vmmd writer in the pre-PR #4 code
--   path never wrote these columns). A NOT NULL constraint would
--   reject the migration on a fleet that has at least one historical
--   row. Nullable + a default of NULL lets the migration land without
--   a backfill and lets the obs handler render "—" for the pre-PR #4
--   rows. New vmmd writers populate both columns on every row.
--
-- Why numeric(5,2) for cpu_pct_60s and not float / int:
--
--   CPU percentage is bounded in [0.00, 100.00] for sane values (a
--   500% reading means the cgroup read is wrong — we cap to 100 in
--   the vmmd writer). numeric(5,2) gives 0.01% granularity, more
--   than enough for an operator dashboard tile, and avoids float
--   precision concerns in the SQL rollup. The 5-digit total budget
--   includes 2 decimal places — `999.99` is the upper bound.
--
-- Why bigint for disk_used_bytes:
--
--   Snapshot dir can grow past 2 GB easily (130 MB average target
--   per §6.2 invariant #6; a fleet of 50 apps × 200 MB ≈ 10 GB per
--   node). bigint covers up to 8 EB.
--
-- Migration discipline:
--
--   - ADD COLUMN IF NOT EXISTS (Postgres 9.6+) — replay-safe.
--   - No backfill; the columns are populated by the new vmmd writer
--     on subsequent heartbeats. The pre-existing rows keep NULL and
--     the obs handler renders that as a missing tile (see
--     ObsNodeRow.LatestHeartbeatStats field omitempty decision in
--     pkg/api/obs.go).
--   - Slot 199 (next free after 00198_instance_node_bindings.sql).
--     Verify no parallel PR claims 199 at PR-open time.

alter table compute_node_heartbeats
    add column if not exists cpu_pct_60s numeric(5,2),
    add column if not exists disk_used_bytes bigint;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

alter table compute_node_heartbeats
    drop column if exists disk_used_bytes,
    drop column if exists cpu_pct_60s;

-- +goose StatementEnd
