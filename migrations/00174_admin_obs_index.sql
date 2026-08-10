-- filename: 00174_admin_obs_index.sql
-- +goose Up
-- +goose StatementBegin
-- Issue #777 / ADR-091: the operator observability backend at
-- /v1/admin/obs/* scans a few tables in fleet-wide mode (every org,
-- every build, every event of a given kind). The existing per-tenant
-- indexes do not cover the new access pattern — a full seq scan
-- would not bite on a one-box deployment today but breaks the day a
-- second control plane node joins (the multi-host control plane is
-- the long-term target in CLAUDE.md). Add the four composite /
-- partial indexes now while the table is small; a future PR-A
-- (multi-host) ships without a perf regression on the obs surface.
--
-- Slot 174 is the next free slot on origin/main. Slots 168 through
-- 172 are reserved by sibling PRs we cannot disturb (per ADR-041 /
-- migration-slot-renumber-at-pr-creation memory), and 173 is taken
-- by invocations_outcome — confirm with `git fetch origin main;
-- git ls-tree origin/main migrations | tail` before opening this PR.
--
-- Replay-safe (ADR-041): CREATE INDEX IF NOT EXISTS is idempotent
-- in Postgres; rolling forward re-applies cleanly.
--
-- Index shape picked per `pkg/db/migrate.go` conventions:
--   * orgs_created_at_idx     — supports the tenant-list since-floor
--                               (cursor pagination by created_at DESC).
--   * orgs_status_idx         — partial index on non-'active' rows so
--                               the "X suspended accounts today" KPI
--                               stays cheap as the active base grows.
--   * builds_account_created_idx — composite (account_id, created_at
--                               DESC); covers both per-account and
--                               fleet-wide build scans with one plan.
--   * events_kind_at_idx     — composite (kind, at DESC) for the
--                               rate-limit / anomaly aggregates (PR #2
--                               will reuse this index from the day the
--                               migrations land; PR #1 uses it
--                               transitively when the audit-log search
--                               (PR #3) joins on kind).
CREATE INDEX IF NOT EXISTS orgs_created_at_idx
    ON orgs USING btree (created_at DESC);

CREATE INDEX IF NOT EXISTS orgs_status_idx
    ON orgs USING btree (status)
    WHERE status <> 'active';

CREATE INDEX IF NOT EXISTS builds_account_created_idx
    ON builds USING btree (account_id, created_at DESC);

CREATE INDEX IF NOT EXISTS events_kind_at_idx
    ON events USING btree (kind, at DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS events_kind_at_idx;
DROP INDEX IF EXISTS builds_account_created_idx;
DROP INDEX IF EXISTS orgs_status_idx;
DROP INDEX IF EXISTS orgs_created_at_idx;
-- +goose StatementEnd
