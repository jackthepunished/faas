-- ADR-132 — asynchronous runtime configuration apply workflow.
-- Desired state is deliberately separate from the operation row: one setting
-- can be edited again while an older graceful/rolling request remains in the
-- audit history, and optimistic version checks keep those transitions safe.

-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS runtime_config_operations (
    id uuid PRIMARY KEY,
    config_key text NOT NULL,
    scope text NOT NULL DEFAULT 'global'
        CHECK (scope IN ('global', 'control_plane', 'daemon', 'node')),
    scope_id text NOT NULL DEFAULT '',
    config_version bigint NOT NULL CHECK (config_version > 0),
    desired_value jsonb NOT NULL,
    effective_value jsonb,
    apply_mode text NOT NULL
        CHECK (apply_mode IN ('graceful', 'rolling', 'break_glass')),
    status text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'blocked', 'cancelled')),
    phase text NOT NULL DEFAULT 'queued',
    error text,
    actor_id uuid,
    reason text NOT NULL DEFAULT '',
    target_count integer NOT NULL DEFAULT 0 CHECK (target_count >= 0),
    applied_count integer NOT NULL DEFAULT 0 CHECK (applied_count >= 0),
    failed_count integer NOT NULL DEFAULT 0 CHECK (failed_count >= 0),
    requested_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    finished_at timestamptz
);

CREATE INDEX IF NOT EXISTS runtime_config_operations_requested_idx
    ON runtime_config_operations (status, requested_at);
CREATE INDEX IF NOT EXISTS runtime_config_operations_key_idx
    ON runtime_config_operations (config_key, scope, scope_id, config_version DESC);

CREATE OR REPLACE FUNCTION notify_runtime_config_operation_changed()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify(
        'runtime_config_operation_changed',
        json_build_object(
            'id', NEW.id,
            'config_key', NEW.config_key,
            'config_version', NEW.config_version,
            'status', NEW.status
        )::text
    );
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS runtime_config_operations_notify
    ON runtime_config_operations;
CREATE TRIGGER runtime_config_operations_notify
AFTER INSERT OR UPDATE ON runtime_config_operations
FOR EACH ROW EXECUTE FUNCTION notify_runtime_config_operation_changed();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS runtime_config_operations_notify ON runtime_config_operations;
DROP FUNCTION IF EXISTS notify_runtime_config_operation_changed();
DROP INDEX IF EXISTS runtime_config_operations_key_idx;
DROP INDEX IF EXISTS runtime_config_operations_requested_idx;
DROP TABLE IF EXISTS runtime_config_operations;
-- +goose StatementEnd
