-- +goose Up
-- +goose StatementBegin
-- ADR-099 (proposed) + Mega-1 supplement: extend job_tasks with the
-- fields the dispatch + retry + cancel paths need that the 00255
-- schema left out.
--
-- - exit_code          : captured from the guest's job_exit vsock DGRAM
--                        (port 1026 msg_type 4) so retries can carry
--                        the prior exit forward and dead_letter
--                        classification works.
-- - next_attempt_at    : backoff-delayed retry scheduling.
--                        JobBackoffBaseSeconds * 2^(attempt-1) capped
--                        at JobBackoffMaxSeconds.
--
-- Plus two CHECK constraint relaxations:
--
-- - job_tasks_instance_pair_chk: free `cancelled` to have instance_id
--   IS NULL. A task cancelled before dispatch has no instance.
--   The pair check now reads:
--     queued      → instance_id IS NULL
--     claimed     → instance_id IS NOT NULL
--     terminal    → either (instance_id NULL if cancelled-from-queued)
--
-- - job_tasks_error_class_check: widen vocabulary to include
--   'success', 'cancelled', 'job_paused', 'oom_or_killed' so the
--   engine + reaper + job_exit handler can use the same field.

ALTER TABLE job_tasks
    ADD COLUMN IF NOT EXISTS exit_code int NULL;

ALTER TABLE job_tasks
    ADD COLUMN IF NOT EXISTS next_attempt_at timestamptz NULL;

CREATE INDEX IF NOT EXISTS job_tasks_next_attempt_idx
    ON job_tasks (next_attempt_at)
    WHERE next_attempt_at IS NOT NULL
      AND status IN ('queued', 'claimed');

ALTER TABLE job_tasks
    DROP CONSTRAINT IF EXISTS job_tasks_instance_pair_chk;

ALTER TABLE job_tasks
    ADD CONSTRAINT job_tasks_instance_pair_chk
    CHECK (
        (status = 'queued' AND instance_id IS NULL)
        OR (status = 'claimed' AND instance_id IS NOT NULL)
        OR (status IN ('succeeded', 'failed', 'timeout', 'cancelled', 'oom'))
    );

ALTER TABLE job_tasks
    DROP CONSTRAINT IF EXISTS job_tasks_error_class_check;

ALTER TABLE job_tasks
    ADD CONSTRAINT job_tasks_error_class_check
    CHECK (
        error_class IS NULL
        OR error_class IN (
            'timeout',
            'refused',
            'tls_handshake',
            'dns',
            'unreachable',
            'oom',
            'user_error',
            'infra',
            'success',
            'cancelled',
            'job_paused',
            'oom_or_killed'
        )
    );
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Forward-only: do not drop columns on rollback (memory:
-- migration-gates-collision-and-replay — replay safety).
SELECT 1;
-- +goose StatementEnd
