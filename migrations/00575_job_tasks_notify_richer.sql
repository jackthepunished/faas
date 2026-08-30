-- +goose Up
-- +goose StatementBegin
-- The 00255 trigger only emits 'job_tasks_queued' on INSERT. The
-- Mega-1 dispatch path needs two richer channels:
--   - job_tasks_dispatched : on queued → claimed (engine writes
--                             instance_id + lease_token)
--   - job_tasks_terminal   : on any terminal transition
--                             (succeeded/failed/timeout/oom/cancelled)
--
-- Drop-before-create (memory: trigger-replay-safety-drop-before-create).
-- Payload is versioned ({v:1, ...}) so an old listener ignores new
-- events and a new listener rejects unversioned payloads without
-- crashing.

DROP TRIGGER IF EXISTS job_tasks_notify_trg ON job_tasks;
DROP FUNCTION IF EXISTS job_tasks_notify();

CREATE OR REPLACE FUNCTION job_tasks_notify_v2() RETURNS trigger AS $$
DECLARE
    channel text;
    payload jsonb;
BEGIN
    IF TG_OP = 'INSERT' AND NEW.status = 'queued' THEN
        channel := 'job_tasks_queued';
        payload := jsonb_build_object(
            'v', 1,
            'op', TG_OP,
            'run_id', NEW.run_id,
            'task_index', NEW.task_index,
            'attempt', NEW.attempt
        );
    ELSIF TG_OP = 'UPDATE'
        AND OLD.status = 'queued' AND NEW.status = 'claimed' THEN
        channel := 'job_tasks_dispatched';
        payload := jsonb_build_object(
            'v', 1,
            'op', TG_OP,
            'run_id', NEW.run_id,
            'task_index', NEW.task_index,
            'attempt', NEW.attempt,
            'instance_id', NEW.instance_id,
            'lease_token', NEW.lease_token
        );
    ELSIF TG_OP = 'UPDATE'
        AND OLD.status IN ('queued', 'claimed')
        AND NEW.status IN ('succeeded', 'failed', 'timeout', 'cancelled', 'oom') THEN
        channel := 'job_tasks_terminal';
        payload := jsonb_build_object(
            'v', 1,
            'op', TG_OP,
            'run_id', NEW.run_id,
            'task_index', NEW.task_index,
            'attempt', NEW.attempt,
            'status', NEW.status,
            'exit_code', NEW.exit_code,
            'error_class', NEW.error_class,
            'finished_at', NEW.finished_at
        );
    ELSE
        RETURN NEW;
    END IF;

    PERFORM pg_notify(channel, payload::text);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER job_tasks_notify_trg
    AFTER INSERT OR UPDATE ON job_tasks
    FOR EACH ROW
    EXECUTE FUNCTION job_tasks_notify_v2();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS job_tasks_notify_trg ON job_tasks;
DROP FUNCTION IF EXISTS job_tasks_notify_v2();

CREATE OR REPLACE FUNCTION job_tasks_notify() RETURNS trigger AS $$
BEGIN
    IF (TG_OP = 'INSERT' AND NEW.status = 'queued')
        OR (TG_OP = 'UPDATE' AND OLD.status <> 'queued' AND NEW.status = 'queued') THEN
        PERFORM pg_notify(
            'job_tasks_queued',
            format('%s|%s|%s', NEW.run_id, NEW.task_index, TG_OP)
        );
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER job_tasks_notify_trg
    AFTER INSERT OR UPDATE ON job_tasks
    FOR EACH ROW
    EXECUTE FUNCTION job_tasks_notify();
-- +goose StatementEnd
