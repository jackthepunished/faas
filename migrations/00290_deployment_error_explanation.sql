-- +goose Up
-- +goose StatementBegin

-- Error-explanations cluster (spec §6.4 amendment 1, ADR-110
-- amendment 1): durable carrier for the customer-facing prose that
-- decorates every failure on the wire (hint / why / fix /
-- relevant_logs). Sibling to migrations/00021_deployment_error_code.sql,
-- which carries the RFC 7807 stable code itself; the four columns
-- here carry the human-readable explanation so post-mortem retrieval
-- works via `gregale inspect <slug> --errors` and `gregale logs
-- <slug> --deployment <id> --explain` after the request that
-- surfaced the original Problem has long since been GC'd from the
-- gateway's response cache.
--
-- Why 4 columns instead of one jsonb:
--
--   1. The text columns (error_hint, error_why, error_fix) are
--      queried individually by the CLI inspector + dashboard; a
--      jsonb column would force every read site to JSON-decide
--      before it can render the field.
--   2. error_relevant_logs IS jsonb because the per-line shape
--      (ts/level/source/message) is variable-length and only ever
--      read whole — never queried by individual field — so a single
--      jsonb blob is correct and avoids a join table for what is
--      essentially a 5-20 element array on the failure path.
--   3. NULL on every existing row; backfill N/A — old failed
--      deployments simply render with empty hint/why/fix, which the
--      catalog lookup in pkg/whycopy.Decorate already handles as a
--      no-op (the catalog row overwrites empty fields).
--
-- Idempotent (IF NOT EXISTS) so the migration can be re-run during
-- local development. Mirrors the migration 00021 pattern exactly
-- (add column if not exists + drop column if exists on downgrade).

alter table deployments
  add column if not exists error_hint text,
  add column if not exists error_why  text,
  add column if not exists error_fix  text,
  add column if not exists error_relevant_logs jsonb;

-- Indexes: the dashboard's "deployments that failed in the last 24h,
-- grouped by error_code" query (which already uses
-- deployments_failed_error_code_idx from 00021) gains nothing from
-- an additional hint/why/fix index — those columns are never
-- GROUP BY targets, only individual-row fetches on a single
-- deployment ID. No new indexes here. error_relevant_logs is a
-- jsonb blob read whole, so no index either.

-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin
alter table deployments drop column if exists error_relevant_logs;
alter table deployments drop column if exists error_fix;
alter table deployments drop column if exists error_why;
alter table deployments drop column if exists error_hint;
-- +goose StatementEnd
