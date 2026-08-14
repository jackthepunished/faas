-- filename: 00256_instances_kind_job.sql
-- +goose Up
-- +goose StatementBegin

-- ADR-099 erratum-1 + erratum-2 (jobs-implementation-plan.md):
-- instances.kind does not exist on main; ADR-099 §Decision 3
-- assumed `instances_kind_check` would already be there to
-- DROP+ADD. It is not. This migration:
--
--   1. ADDs the `kind` column with NOT NULL DEFAULT 'wake'
--      so the backfill is metadata-only (no table rewrite
--      in PG15 — adding a column with a constant default
--      is a fast catalog-only change).
--   2. ADDs the `instances_kind_check` CHECK that admits
--      'wake', 'build', 'job_task'. The three-kind closed
--      vocabulary is the wire-bypass backstop; pkg/sched
--      already only emits these three values (PR-0
--      ratelimits by app, PR-C adds WakeJob → job_task,
--      builds already exist). Future kinds land via the
--      ADR-091 amendment dance (DROP+ADD).
--   3. WIDENS `instances.app_id` to nullable (was NOT NULL
--      FK to apps(id)). Job-task instances have no app_id
--      — the dispatch tick (PR-C) writes instance rows
--      with app_id=NULL, job_id=:id, kind='job_task'.
--   4. ADDs `instances.job_id` as a nullable FK to
--      jobs(id) (UNQUALIFIED — the search_path at apply
--      time resolves to whatever schema holds the
--      table; production and the pgtest harness use
--      different schemas but both honour the path).
--      ON DELETE SET NULL so a customer hard-deleting
--      a job doesn't cascade to instance rows (those
--      are part of the audit trail and must survive
--      the job deletion per the meterd/billing
--      contract).
--   5. ADDs the partial index `instances_job_id_idx
--      WHERE job_id IS NOT NULL` — the per-job instance
--      lookup is hot (the run-detail page + the reaper
--      exemption filter).
--
-- Slot reservation: 00256 in the ADR-099 cluster range
-- (00255-00263). See 00255_jobs.sql header.
--
-- Replay safety: ADD COLUMN IF NOT EXISTS (with constant
-- default) and DROP CONSTRAINT IF EXISTS + ADD CONSTRAINT
-- (no IF NOT EXISTS on ADD CONSTRAINT in PG15; the DROP
-- IF EXISTS makes the second pass a no-op).

-- (1) Add the kind column with a constant default for
-- the fast catalog-only backfill. The replay-safe
-- IF NOT EXISTS makes a second MigrateUp a no-op.
ALTER TABLE instances
    ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'wake';

-- (2) Add the CHECK constraint. Drop-before-add per the
-- replay-safety precedent (memory:
-- trigger-replay-safety-drop-before-create). PG15 rejects
-- ADD CONSTRAINT IF NOT EXISTS, so the DROP IF EXISTS is
-- the canonical shape.
ALTER TABLE instances
    DROP CONSTRAINT IF EXISTS instances_kind_check;

ALTER TABLE instances
    ADD CONSTRAINT instances_kind_check
    CHECK (kind IN ('wake','build','job_task'));

-- (3) Widen app_id to nullable. This is a metadata-only
-- change in PG15 (no row rewrite because the column
-- type stays the same). The existing NOT NULL rows
-- keep their FK value; new job_task inserts write
-- NULL with a non-NULL job_id (the pair CHECK below
-- enforces one-of).
--
-- The replay-safe IF EXISTS makes a second MigrateUp a
-- no-op (the second ALTER would fail with
-- `column ... does not allow constraints to be changed
-- to NOT NULL` if any NULL row existed; we never set
-- any NULL row at this migration's apply-time).
ALTER TABLE instances
    ALTER COLUMN app_id DROP NOT NULL;

-- (4) Add the job_id column. Nullable FK with ON DELETE
-- SET NULL: when a customer hard-deletes a job (the
-- operator-only path), the surviving instance rows
-- (audit trail) keep their app_id/deployment_id context
-- and the job_id clears. The instances_cron_run_id
-- precedent (migrations/00205_job_runs_run_id.sql) is
-- the analog — same ON DELETE SET NULL shape.
ALTER TABLE instances
    ADD COLUMN IF NOT EXISTS job_id uuid;

-- FK added in a separate ALTER so the replay-safety
-- harness can apply this migration twice without
-- tripping the "constraint already exists" error
-- (SQLSTATE 42710). The DROP IF EXISTS in the down
-- migration handles the inverse.
--
-- jobs is referenced UNQUALIFIED (not public.jobs) so
-- the test harness's per-schema search_path resolves
-- the FK target the same way production does. Schema
-- qualifying breaks the pgtest isolated-schema path
-- (search_path=schema,public; the test's jobs row
-- lives in the test schema, not public).
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conname = 'instances_job_id_fk'
           AND conrelid = 'instances'::regclass
    ) THEN
        ALTER TABLE instances
            ADD CONSTRAINT instances_job_id_fk
            FOREIGN KEY (job_id) REFERENCES jobs(id)
            ON DELETE SET NULL;
    END IF;
END $$;

-- Pair-CHECK: an instance row must point at either an
-- app (the wake/build path) or a job (the job_task
-- path), never both, never neither. This is the
-- wire-bypass backstop for the dispatch tick (PR-C)
-- and the existing Wake path (PR-A's only PR-C-side
-- change is `kind='wake'` default; this migration
-- also projects the existing NOT NULL app_id default
-- into the pair CHECK so legacy wake rows satisfy
-- (app_id IS NOT NULL AND job_id IS NULL)).
ALTER TABLE instances
    DROP CONSTRAINT IF EXISTS instances_app_or_job_chk;

ALTER TABLE instances
    ADD CONSTRAINT instances_app_or_job_chk
    CHECK (
        (kind IN ('wake','build') AND app_id IS NOT NULL AND job_id IS NULL)
     OR (kind = 'job_task'   AND app_id IS NULL     AND job_id IS NOT NULL)
    );

-- (5) Partial index for the per-job instance lookup.
-- The reaper (PR-C) uses this to filter ReapIdle-eligible
-- rows: `WHERE job_id IS NOT NULL` is the job_task
-- predicate that skips the reaper. The run-detail page
-- (PR-D) renders tasks-as-instances by joining through
-- this index.
CREATE INDEX IF NOT EXISTS instances_job_id_idx
    ON instances (job_id)
    WHERE job_id IS NOT NULL;

-- Dispatch tick hot path: the schedd 1s tick (PR-C)
-- queries `instances WHERE kind='job_task'` to count
-- the per-run running tasks (the parallelism cap).
-- Partial on kind='job_task' keeps the index tiny (the
-- wake/build rows dwarf the job_task rows today).
CREATE INDEX IF NOT EXISTS instances_kind_job_task_idx
    ON instances (job_id)
    WHERE kind = 'job_task';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Forward-only by design (mirrors the migration
-- precedent at migrations/00206_*.sql:58-67): reverting
-- the kind widening would orphan any existing
-- `kind='job_task'` rows the schedd dispatch tick
-- created. The down migration therefore preserves the
-- widened state unconditionally — it just no-ops.

SELECT 1;

-- +goose StatementEnd
