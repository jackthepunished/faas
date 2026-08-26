-- +goose Up
-- Operator runtime configuration (ADR-132).
--
-- The row is the durable desired state. Daemons keep an in-memory
-- effective snapshot and acknowledge application back into this table.
-- pg_notify is only the low-latency wake-up path; a daemon must always
-- reconcile from this table on boot and after reconnecting to Postgres.

CREATE TABLE IF NOT EXISTS runtime_config_entries (
    id              uuid PRIMARY KEY,
    config_key      text NOT NULL,
    scope           text NOT NULL DEFAULT 'global'
                    CHECK (scope IN ('global', 'control_plane', 'daemon', 'node')),
    scope_id        text NOT NULL DEFAULT '',
    desired_value   jsonb NOT NULL,
    effective_value jsonb NULL,
    version         bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    apply_mode      text NOT NULL
                    CHECK (apply_mode IN ('hot', 'graceful', 'rolling', 'break_glass')),
    status          text NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'applied', 'failed', 'blocked')),
    last_error      text NULL,
    actor_id        uuid NULL,
    reason          text NULL,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    applied_at      timestamptz NULL,
    UNIQUE (config_key, scope, scope_id)
);

CREATE INDEX IF NOT EXISTS runtime_config_entries_scope_idx
    ON runtime_config_entries (scope, scope_id, config_key);

CREATE TABLE IF NOT EXISTS runtime_config_revisions (
    id              bigserial PRIMARY KEY,
    entry_id        uuid NOT NULL REFERENCES runtime_config_entries(id) ON DELETE CASCADE,
    config_key      text NOT NULL,
    scope           text NOT NULL,
    scope_id        text NOT NULL,
    version         bigint NOT NULL,
    old_value       jsonb NULL,
    new_value       jsonb NOT NULL,
    actor_id        uuid NULL,
    reason          text NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (entry_id, version)
);

CREATE INDEX IF NOT EXISTS runtime_config_revisions_lookup_idx
    ON runtime_config_revisions (config_key, scope, scope_id, created_at DESC);

-- A small durable event lets every daemon reconcile after a missed notify.
CREATE OR REPLACE FUNCTION notify_runtime_config_changed()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    PERFORM pg_notify(
        'runtime_config_changed',
        json_build_object(
            'key', NEW.config_key,
            'scope', NEW.scope,
            'scope_id', NEW.scope_id,
            'version', NEW.version
        )::text
    );
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS runtime_config_entries_notify ON runtime_config_entries;
CREATE TRIGGER runtime_config_entries_notify
AFTER INSERT OR UPDATE ON runtime_config_entries
FOR EACH ROW EXECUTE FUNCTION notify_runtime_config_changed();

-- +goose Down
DROP TRIGGER IF EXISTS runtime_config_entries_notify ON runtime_config_entries;
DROP FUNCTION IF EXISTS notify_runtime_config_changed();
DROP INDEX IF EXISTS runtime_config_revisions_lookup_idx;
DROP TABLE IF EXISTS runtime_config_revisions;
DROP INDEX IF EXISTS runtime_config_entries_scope_idx;
DROP TABLE IF EXISTS runtime_config_entries;
