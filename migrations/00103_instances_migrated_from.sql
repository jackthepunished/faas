-- +goose Up
-- +goose StatementBegin

-- filename: 00103_instances_migrated_from.sql
--
-- Tier A5 — additive migration lineage columns on `instances`
-- (ADR-065 cross-node live-instance migration, follow-up to
-- ADR-064).
--
-- `migrated_from_node_id` carries the previous owner after a
-- live-instance handoff (two-phase commit: dying vmmd pauses
-- + snapshots, new owner create-from-snapshots). The FK to
-- `compute_nodes(id)` with ON DELETE SET NULL keeps the
-- reference honest when a node is decommissioned — the row
-- stays, but `migrated_from_node_id` flips to NULL and
-- `migrated_at` stays as the historical stamp. This matches
-- ADR-009 (compute_nodes immutable identity) — the lineage
-- column is informational, not authoritative.
--
-- `migrated_at` is the wall-clock stamp of the commit (Phase
-- 3 of the four-phase handoff). Nullable at insert; a fresh
-- instance has never been migrated. The clock-skew CHECK
-- `migrated_at <= now() + interval '1 minute'` tolerates
-- minor NTP drift across schedds; values clearly in the
-- future still error loud (23514, ADR-005 / 00095 pattern).
--
-- `lease_token` is the per-migration UUID minted by the new
-- owner at Phase 1 of the four-phase handoff. It is part of
-- the conditional-UPDATE predicate at Phase 3 (commit), so
-- a peer claim can never silently succeed with a stale
-- lease. The column is also the lookup key for the dying
-- vmmd's pause-resume lease bookkeeping (the dying vmmd
-- resumes the VM on lease expiry). Mirrors the A4
-- `apps.reassigned_at` schema discipline.
--
-- Replay-safety: every ADD COLUMN is paired with a DROP
-- COLUMN in the down block; the constraint add is paired
-- with DROP CONSTRAINT IF EXISTS (PR #377 / ADR-041
-- contract). The partial index uses IF NOT EXISTS / DROP
-- INDEX IF EXISTS for the same reason.

alter table instances
  add column if not exists migrated_from_node_id uuid
    references compute_nodes(id) on delete set null,
  add column if not exists migrated_at timestamptz,
  add column if not exists lease_token text,
  add constraint instances_migrated_at_chk
    check (migrated_at is null
           or migrated_at <= now() + interval '1 minute');

create index if not exists instances_migrated_from_node_id_idx
  on instances (migrated_from_node_id)
  where migrated_from_node_id is not null;

-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin

drop index if exists instances_migrated_from_node_id_idx;
alter table instances drop constraint if exists instances_migrated_at_chk;
alter table instances drop column if exists lease_token;
alter table instances drop column if exists migrated_at;
alter table instances drop column if exists migrated_from_node_id;

-- +goose StatementEnd