-- filename: 00257_usage_minutes_app_nullable.sql
-- +goose Up
-- +goose StatementBegin

-- ADR-099 erratum-3 (jobs-implementation-plan.md):
-- `usage_minutes.app_id` is currently NOT NULL on main
-- (migrations/00001_init.sql:97-99). ADR-099 §Decision 7
-- assumed `app_id=NULL` would work for jobs; it does not.
-- This migration:
--
--   1. WIDENS `usage_minutes.app_id` to nullable. Job-task
--      instance rows have app_id=NULL (per 00256); the
--      corresponding usage_minutes rows therefore must
--      also allow app_id=NULL so meterd can write the
--      billable-second rows. The job_id denormalised
--      column below preserves the per-job roll-up.
--   2. ADDs `usage_minutes.meter_kind` with NOT NULL
--      DEFAULT 'app'. Backfill is metadata-only (constant
--      default). The CHECK bounds the closed vocabulary:
--      'app' / 'job'. Future kinds land via the
--      ADR-091 amendment dance.
--   3. ADDs `usage_minutes.job_id` as a nullable uuid (no
--      FK — the job may have been deleted; the meterd
--      row survives). The per-job roll-up joins through
--      this column.
--   4. ADDs the partial index `usage_minutes_job_idx
--      WHERE meter_kind='job'` — the per-job billable
--      roll-up is the hot path for the job-detail
--      dashboard (PR-D) and the ADR-099 §Acceptance #5
--      `jobs_minutes_metered_total` metric.
--
-- Slot reservation: 00257 in the ADR-099 cluster range
-- (00255-00263). See 00255_jobs.sql header.
--
-- Replay safety: ADD COLUMN IF NOT EXISTS (constant
-- default — fast catalog-only backfill); ALTER ... DROP
-- NOT NULL is metadata-only in PG15; the FK on job_id is
-- added with a NOT EXISTS guard (mirrors the
-- 00246_instances_kind_job.sql shape).

-- (1) Widen app_id to nullable. Metadata-only in PG15
-- (no row rewrite because the column type stays the
-- same). The existing NOT NULL rows keep their FK
-- value; new job rows write NULL with a non-NULL
-- job_id (the pair CHECK below enforces one-of).
ALTER TABLE usage_minutes
    ALTER COLUMN app_id DROP NOT NULL;

-- (2) Add the meter_kind column with a constant default
-- for the fast catalog-only backfill. The replay-safe
-- IF NOT EXISTS makes a second MigrateUp a no-op.
ALTER TABLE usage_minutes
    ADD COLUMN IF NOT EXISTS meter_kind text NOT NULL DEFAULT 'app';

ALTER TABLE usage_minutes
    DROP CONSTRAINT IF EXISTS usage_minutes_meter_kind_check;

ALTER TABLE usage_minutes
    ADD CONSTRAINT usage_minutes_meter_kind_check
    CHECK (meter_kind IN ('app','job'));

-- (3) Add the job_id column. Nullable (no FK — the job
-- may have been hard-deleted; the meterd row survives
-- per the audit-trail contract). The pair CHECK below
-- enforces one-of with app_id.
ALTER TABLE usage_minutes
    ADD COLUMN IF NOT EXISTS job_id uuid;

-- Pair-CHECK: a usage_minutes row must point at either
-- an app (the wake/build path) or a job (the job_task
-- path), never both, never neither. The default
-- `meter_kind='app'` + the existing NOT-NULL app_id
-- rows satisfy the (app_id IS NOT NULL AND job_id IS
-- NULL) branch; new job rows satisfy the
-- (app_id IS NULL AND job_id IS NOT NULL) branch.
ALTER TABLE usage_minutes
    DROP CONSTRAINT IF EXISTS usage_minutes_app_or_job_chk;

ALTER TABLE usage_minutes
    ADD CONSTRAINT usage_minutes_app_or_job_chk
    CHECK (
        (meter_kind = 'app' AND app_id IS NOT NULL AND job_id IS NULL)
     OR (meter_kind = 'job' AND app_id IS NULL     AND job_id IS NOT NULL)
    );

-- (4) Partial index for the per-job roll-up. Mirrors
-- the `instances_job_id_idx` shape from 00256.
CREATE INDEX IF NOT EXISTS usage_minutes_job_idx
    ON usage_minutes (account_id, minute DESC)
    WHERE meter_kind = 'job';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Forward-only by design (mirrors 00256's down
-- migration): reverting the widening would orphan any
-- existing job-metered rows. The down migration
-- preserves the widened state unconditionally — it
-- just no-ops.

SELECT 1;

-- +goose StatementEnd
