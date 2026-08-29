-- +goose Up
-- +goose StatementBegin
-- Issue #1184 Workstream A explicitly requires "lease ownership and
-- idempotency" for job tasks. ADR-099 Decision 6 mentions lease
-- ownership without specifying a schema; the shipped 00255 schema
-- has none.
--
-- Add the minimal lease columns so the dispatch tick (M5) can:
--   - mint a fresh UUIDv7 lease_token at MarkTaskClaimed
--   - validate the lease_token on every incoming job_exit DGRAM
--   - expire the lease on task_timeout_s + grace so a dead schedd's
--     leases can be stolen by the reaper / restarted schedd
--   - track last_lease_node for observability + audits

ALTER TABLE job_tasks
    ADD COLUMN IF NOT EXISTS lease_token uuid NULL;

ALTER TABLE job_tasks
    ADD COLUMN IF NOT EXISTS lease_expires_at timestamptz NULL;

ALTER TABLE job_tasks
    ADD COLUMN IF NOT EXISTS last_lease_node text NULL;

CREATE UNIQUE INDEX IF NOT EXISTS job_tasks_lease_uniq
    ON job_tasks (lease_token)
    WHERE lease_token IS NOT NULL;

CREATE INDEX IF NOT EXISTS job_tasks_lease_expires_idx
    ON job_tasks (lease_expires_at)
    WHERE lease_token IS NOT NULL
      AND status IN ('queued', 'claimed');

ALTER TABLE job_runs
    ADD COLUMN IF NOT EXISTS dead_letter_count int NOT NULL DEFAULT 0;

-- job_runs_counters_check is the denormalised-counter check from
-- 00255. Drop+recreate to include dead_letter_count and the
-- cross-field invariant (sum ≤ tasks).
DO $$
DECLARE
    c text;
BEGIN
    SELECT conname INTO c FROM pg_constraint
    WHERE conname = 'job_runs_counters_check'
      AND conrelid = 'public.job_runs'::regclass;
    IF c IS NOT NULL THEN
        ALTER TABLE job_runs DROP CONSTRAINT job_runs_counters_check;
    END IF;
END $$;

ALTER TABLE job_runs
    ADD CONSTRAINT job_runs_counters_check
    CHECK (
        tasks >= 0
        AND tasks_succeeded >= 0
        AND tasks_failed >= 0
        AND tasks_cancelled >= 0
        AND tasks_running >= 0
        AND dead_letter_count >= 0
        AND dead_letter_count <= tasks
        AND tasks_succeeded + tasks_failed + tasks_cancelled + tasks_running + dead_letter_count <= tasks
    );
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
