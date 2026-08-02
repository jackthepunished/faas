-- +goose Up
-- +goose StatementBegin

-- filename: 00095_apps_reassigned_at.sql
--
-- Tier A4 — durable cooldown timestamp for cross-node app
-- reassignment (ADR-064, follow-up to ADR-062).
--
-- Pre-00095 the apps table has no reassigned_at column: once
-- apps.node_id is set by the Phase-2 / Gate A claim path
-- (migration 00091), it is immutable. Tier A4's rebalancer
-- (pkg/sched/rebalancer.go) reassigns active/evicted_cold apps
-- from a dead compute_node to a live peer via the conditional
-- UPDATE in Store.ReassignAppOwner — but a flap-loop
-- (operator toggles compute_nodes.active=false / true
-- rapidly) could otherwise pin a single app in flight between
-- peers indefinitely.
--
-- The column carries the wall-clock time of the most recent
-- successful reassignment. The rebalancer's
-- ListOrphanedApps SQL filters
--   reassigned_at IS NULL
--     OR reassigned_at < now() - interval '<cooldown>s'
-- so a freshly-reassigned app stays on its current owner for
-- at least RebalanceCooldownSeconds (default 60s, env-
-- overridable via FAAS_REBALANCE_COOLDOWN_SECONDS; the
-- constant lives in pkg/api/limits.go). The CHECK tolerates
-- minor NTP drift across nodes
-- (reassigned_at <= now() + interval '1 minute'); values
-- clearly in the future still error loud.
--
-- No backfill: the column is nullable, and pre-00095 rows
-- are the legacy "never reassigned" state, which the
-- rebalancer reads as "always eligible".
--
-- Partial index on reassigned_at WHERE reassigned_at IS NOT
-- NULL — the rebalancer's hot filter is "non-NULL AND < now() -
-- cooldown"; a NULL app must be returned. Indexing only the
-- non-NULL tail keeps the index narrow (a busy fleet has at
-- most one reassignment per app per cooldown window).
--
-- Replay-safety: every ADD COLUMN is IF NOT EXISTS, every
-- constraint add is paired with a DROP IF EXISTS in the down
-- block, every index add is paired with DROP INDEX IF EXISTS.
-- A second MigrateUp() is a no-op (PR #377 / ADR-041
-- contract).

alter table apps
  add column if not exists reassigned_at timestamptz;

alter table apps
  drop constraint if exists apps_reassigned_at_chk;

alter table apps
  add constraint apps_reassigned_at_chk
    check (reassigned_at is null or reassigned_at <= now() + interval '1 minute');

create index if not exists apps_reassigned_at_idx
  on apps (reassigned_at)
  where reassigned_at is not null;

-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin

-- Reverse order of creation. The IF EXISTS guards on the
-- DROP CONSTRAINT calls are belt-and-braces against a half-
-- applied down path; the column + index DROPs (without IF
-- EXISTS in spirit — guarded with IF EXISTS here for
-- idempotency) are the load-bearing ones.

drop index if exists apps_reassigned_at_idx;

alter table apps
  drop constraint if exists apps_reassigned_at_chk;

alter table apps
  drop column if exists reassigned_at;

-- +goose StatementEnd