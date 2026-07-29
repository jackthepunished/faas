-- +goose Up
-- +goose StatementBegin

-- filename: 00071_compute_nodes_region_zone.sql
--
-- 00071_compute_nodes_region_zone.sql — multi-box placement scheduler
-- (ADR-025/028/029, scale-out worktree). Renumbered twice during PR
-- review (00067 → 00069 → 00071) as PR #428 (wip: extend metering
-- telemetry, ADR-048) expanded its migration set to claim slots
-- 67/68/69/70 while #429 was being reviewed. Slot 71 is the next
-- free one. The semantic content is unchanged across renumbers —
-- only the slot moved. Per ADR-041, the renumber commits carry
-- reservations at the abandoned slots (00067_reserve_slot.sql,
-- 00068_reserve_slot.sql, 00069_reserve_slot.sql,
-- 00070_reserve_slot.sql) so the embedded FS stays contiguous and
-- TestMigrationsContiguous passes.
--
-- Two additive columns on compute_nodes:
--   region  — free-form locality label (e.g. "eu-fsn1", "us-east",
--             "local"). Used by pkg/sched/placement.go's chooser as
--             the secondary tie-break when two nodes have identical
--             RAM headroom; sticky-warm affinity lives on the
--             in-memory WarmAffinity cache (schedd-only, no DB).
--   zone    — finer-grained locality inside a region (e.g. "a",
--             "b"). Currently informational; lets a future per-zone
--             scheduler split capacity without a schema change.
--
-- The columns are nullable so pre-00071 schedd rows (the seeded
-- 'default-local' row, plus any operator-added rows since 00024)
-- accept the schema without a separate backfill transaction. This
-- migration backfills the seeded default-local row to ('local',
-- 'local') so the partial index covers it and the chooser has a
-- deterministic tie-break ordering on the single-box deployment.
--
-- No new admission or billing semantics in this migration. The
-- authoritative caps stay in pkg/api/limits.go (CLAUDE.md). The
-- per-row admission_ceiling_mb (00024) already drives schedd's
-- NodeLedger ceiling via pkg/sched/admission.go:ceilingForNode_locked.
--
-- A partial index on (region, zone) WHERE active supports the
-- chooser's filter-and-sort scan: 00024 already created
-- compute_nodes_active_idx on (name) WHERE active; this index
-- covers the locality lookup without forcing a full scan. The
-- selectivity is low at fleet scale (handful of regions) so the
-- index is mostly to keep planner stats stable — at N nodes the
-- cost is O(N log N) with index vs O(N) without.
--
-- Out of scope (mirrors 00024's posture):
--   - apps.preferred_node_id / apps.placement_policy / per-app
--     affinity. The picker biases by WarmAffinity (in-memory)
--     and by per-node RAM headroom; per-app hints are a future
--     ADR if customers ask.
--   - pkg/sched/placement.go schema change is NOT this migration's
--     job; placement.go's Node struct gains Region/Zone fields but
--     those are populated from these columns at runtime, not written
--     back.
--   - snapshots.node_id. Snapshot reuse (ADR-009) is preserved by
--     design — sticky-warm affinity biases the chooser, it does
--     not gate it (ADR-005: cold boot must always work).

alter table compute_nodes
    add column if not exists region text null,
    add column if not exists zone   text null;

comment on column compute_nodes.region is
    'Locality label for the chooser tie-break (pkg/sched/placement.go). Free-form text; nullable so pre-00071 rows accept the schema. The seeded default-local row is backfilled to ''local''. ADR-025.';

comment on column compute_nodes.zone is
    'Finer locality inside region. Currently informational; nullable. ADR-025.';

-- Backfill the seeded default-local row so the single-box deploy
-- has deterministic tie-break ordering. Operators that registered
-- additional compute_nodes since 00024 are not backfilled here —
-- those rows either (a) already have region/zone set by the
-- operator, or (b) rely on the chooser falling through to lex
-- tie-break on name, which is the pre-00071 behaviour.
update compute_nodes
   set region = 'local',
       zone   = 'local'
 where name = 'default-local'
   and region is null;

-- Partial index for the (region, zone) scan inside the chooser.
-- Matches the existing 00024 pattern: WHERE active = true keeps
-- inactive rows out of the planner stats.
create index if not exists compute_nodes_region_zone_idx
    on compute_nodes (region, zone)
 where active = true;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Clean reversal. Drop the index first so the column drop doesn't
-- have to worry about dangling index references. Default-local
-- backfill is implicitly undone (region and zone return to NULL).
-- This matches the 00010/00013 posture: down-migrations on shipped
-- platforms require a manual runbook, but the SQL here is a clean
-- reverse of Up.
drop index if exists compute_nodes_region_zone_idx;

alter table compute_nodes
    drop column if exists zone,
    drop column if exists region;

-- +goose StatementEnd