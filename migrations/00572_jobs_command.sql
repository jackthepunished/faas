-- +goose Up
-- +goose StatementBegin
-- ADR-099 Decision 1 + Mega-1 supplement: jobs.command text[] is the
-- customer-facing entrypoint. The 00255 schema left this column off;
-- the dispatch tick (M5) refuses to dispatch any task of a job with
-- empty command — fails closed.
--
-- Pre-existing jobs (created between 00255 and 00526) are backfilled
-- with a fail-closed placeholder so the cluster stays consistent and
-- the customer sees clear stderr when they PATCH their job.

ALTER TABLE jobs
    ADD COLUMN IF NOT EXISTS command text[] NOT NULL DEFAULT '{}';

ALTER TABLE jobs
    DROP CONSTRAINT IF EXISTS jobs_command_min_chk;

ALTER TABLE jobs
    ADD CONSTRAINT jobs_command_min_chk
    CHECK (array_length(command, 1) IS NULL
        OR array_length(command, 1) BETWEEN 0 AND 64);

UPDATE jobs
    SET command = ARRAY['/bin/sh', '-c', 'echo "no command configured; PATCH your job to set command[]" 1>&2; exit 127']::text[]
    WHERE command = '{}'::text[];

ALTER TABLE jobs
    ALTER COLUMN command DROP DEFAULT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
