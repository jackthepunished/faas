-- +goose Up
-- +goose StatementBegin
-- The 00256 instances_kind_job migration set instances.job_id FK to
-- ON DELETE SET NULL — but the pair check requires kind='job_task'
-- AND job_id IS NOT NULL. Hard-deleting a job would violate the
-- check on its surviving instance rows.
--
-- Resolution: ON DELETE RESTRICT (the soft-delete path via
-- jobs.status='deleted' + the live-instances check from 00530 is
-- the customer-facing delete). Hard delete via the FK is now blocked.

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'instances_job_id_fk'
    ) THEN
        ALTER TABLE instances DROP CONSTRAINT instances_job_id_fk;
    END IF;
END $$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'instances_job_id_fk'
    ) THEN
        ALTER TABLE instances
            ADD CONSTRAINT instances_job_id_fk
            FOREIGN KEY (job_id) REFERENCES jobs(id) ON DELETE RESTRICT;
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS instances_job_active_idx
    ON instances (job_id)
    WHERE kind = 'job_task'
      AND state NOT IN ('parked', 'destroyed');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
