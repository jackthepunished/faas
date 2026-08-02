-- +goose Up
-- +goose StatementBegin

-- filename: 00104_apps_migrated_at.sql
--
-- Tier A5 — additive migration timestamp on `apps`
-- (ADR-065 cross-node live-instance migration, follow-up to
-- ADR-064).
--
-- `apps.migrated_at` is the wall-clock stamp of the
-- last LIVE-INSTANCE migration commit for this app
-- (different from `apps.reassigned_at` which carries the
-- PARKED-app rebalance commit, ADR-064). Both columns can
-- coexist on the same app — an app whose instances
-- migrated live last week AND whose owner was rebalanced
-- to a new node via the A4 parked path yesterday has
-- both stamps set.
--
-- Nullable at insert; a fresh app has never been migrated.
-- The clock-skew CHECK mirrors 00095 / 00103:
-- `migrated_at <= now() + interval '1 minute'` tolerates
-- minor NTP drift; values clearly in the future still
-- error loud (23514).
--
-- No partial index — the rebalancer's A4 hot path is on
-- `reassigned_at`; `migrated_at` is telemetry only
-- (dashboard "fleet live-migration throughput" panel).
-- The `apps_node_id_status_partial_idx` from 00096 already
-- covers the JOIN surface; the dashboard query can scan.
--
-- Replay-safety: ADD COLUMN IF NOT EXISTS paired with
-- DROP COLUMN IF EXISTS in the down block; constraint
-- add paired with DROP CONSTRAINT IF EXISTS
-- (PR #377 / ADR-041 contract).

alter table apps
  add column if not exists migrated_at timestamptz;

-- PostgreSQL has no ADD CONSTRAINT IF NOT EXISTS, so guard with
-- pg_constraint existence check (00053_deployments_source_url.sql
-- pattern — PR #377 / ADR-041 contract).
do $$
begin
  if not exists (
    select 1 from pg_catalog.pg_constraint
     where conname = 'apps_migrated_at_chk'
       and conrelid = 'apps'::regclass
  ) then
    alter table apps
      add constraint apps_migrated_at_chk
        check (migrated_at is null
               or migrated_at <= now() + interval '1 minute');
  end if;
end$$;

-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin

alter table apps drop constraint if exists apps_migrated_at_chk;
alter table apps drop column if exists migrated_at;

-- +goose StatementEnd