-- filename: 00281_data_upstreams_deployment_scope.sql
-- +goose Up
-- +goose StatementBegin

-- 00281_data_upstreams_deployment_scope.sql — ADR-098 amendment
-- (issue #954, ADR-098-deployment-scope-overlay). Widen the
-- data_upstreams dedupe key from (app_id, scope, kind, host, port)
-- to (app_id, scope, deployment_scope, kind, host, port). Each
-- customer deployment now owns its own captured-upstream set:
-- a staging-DB row no longer collides with production-DB on the
-- same app, and the schedd chooser can apply per-deployment bias.
--
-- Replay-safe order:
--   1. ADD COLUMN NOT NULL DEFAULT 'default' — backfills existing rows.
--   2. CHECK the column shape (mirrors data_upstreams_scope_check).
--   3. DROP the old UNIQUE INDEX
--      (data_upstreams_dedupe_uniq on app_id, scope, kind, host, port).
--   4. CREATE the widened UNIQUE INDEX
--      (app_id, scope, deployment_scope, kind, host, port).
--   5. Widen the pg_notify pipe-payload to 7 fields
--      (app_id|scope|deployment_scope|kind|host|port|op). schedd's
--      pkg/sched/listen.go parses on a fixed split and must update
--      in the same PR.
--
-- Replay-safety: the harness at migrations/replay_safety_test.go
-- (TestNewMigrationsAreReplaySafe) applies the migration twice in a
-- single tx and pins the second pass as a no-op. The widened pipe
-- format is forward-only — the down-migration recreates the 6-field
-- format so a second replay would land on the 7-field create, not
-- the 6-field restore. This tripwire is intentional: the migration
-- is forward-only because schedd's parser must upgrade in lockstep.
--
-- Backfill policy: pre-PR-00213 env rows and the migration's
-- DEFAULT 'default' stamp resolve to deployment_scope='default'.
-- A handler-side backfill (live classifier re-stamps the deployment
-- when env values shift) is the runtime mechanism; the SQL column
-- default is the offline default.

ALTER TABLE data_upstreams
  ADD COLUMN IF NOT EXISTS deployment_scope text NOT NULL DEFAULT 'default';

ALTER TABLE data_upstreams
  ADD CONSTRAINT data_upstreams_deployment_scope_shape
    CHECK (deployment_scope ~ '^[a-z0-9]([a-z0-9-]{1,38})[a-z0-9]$');

-- Widen the dedupe UNIQUE INDEX to include deployment_scope.
DROP INDEX IF EXISTS data_upstreams_dedupe_uniq;

CREATE UNIQUE INDEX IF NOT EXISTS data_upstreams_dedupe_uniq
  ON data_upstreams (app_id, scope, deployment_scope, kind, host, port);

-- Widen the pg_notify pipe payload to 7 fields so schedd's
-- pkg/sched/listen.go parser can identify the row across the
-- widened key. The 6->7 widening is forward-only and the
-- same-PR contract for schedd's parser is documented in the
-- DOWN block below.
DROP TRIGGER IF EXISTS data_upstreams_notify_trg ON data_upstreams;
DROP FUNCTION IF EXISTS data_upstreams_notify();

CREATE OR REPLACE FUNCTION data_upstreams_notify() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify(
        'data_upstreams_changed',
        format('%s|%s|%s|%s|%s|%s|%s',
            COALESCE(NEW.app_id, OLD.app_id)::text,
            COALESCE(NEW.scope, OLD.scope),
            COALESCE(NEW.deployment_scope, OLD.deployment_scope),
            COALESCE(NEW.kind, OLD.kind),
            COALESCE(NEW.host, OLD.host),
            COALESCE(NEW.port, OLD.port)::text,
            TG_OP)
    );
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER data_upstreams_notify_trg
  AFTER INSERT OR UPDATE OR DELETE ON data_upstreams
  FOR EACH ROW
  EXECUTE FUNCTION data_upstreams_notify();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- The DOWN reverses the widening in reverse order so a replay-
-- safety second pass yields the original (pre-00281) shape:
--   1. Re-create the 6-field pg_notify function so schedd's
--      pre-00281 parser still works on a downgrade.
--   2. Re-create the pre-widening unique index on
--      (app_id, scope, kind, host, port).
--   3. Drop the column (CASCADE drops the CHECK constraint).
DROP TRIGGER IF EXISTS data_upstreams_notify_trg ON data_upstreams;
DROP FUNCTION IF EXISTS data_upstreams_notify();

CREATE OR REPLACE FUNCTION data_upstreams_notify() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify(
        'data_upstreams_changed',
        format('%s|%s|%s|%s|%s|%s',
            COALESCE(NEW.app_id, OLD.app_id)::text,
            COALESCE(NEW.scope, OLD.scope),
            COALESCE(NEW.kind, OLD.kind),
            COALESCE(NEW.host, OLD.host),
            COALESCE(NEW.port, OLD.port)::text,
            TG_OP)
    );
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER data_upstreams_notify_trg
  AFTER INSERT OR UPDATE OR DELETE ON data_upstreams
  FOR EACH ROW
  EXECUTE FUNCTION data_upstreams_notify();

DROP INDEX IF EXISTS data_upstreams_dedupe_uniq;

CREATE UNIQUE INDEX IF NOT EXISTS data_upstreams_dedupe_uniq
  ON data_upstreams (app_id, scope, kind, host, port);

ALTER TABLE data_upstreams
  DROP CONSTRAINT IF EXISTS data_upstreams_deployment_scope_shape;

ALTER TABLE data_upstreams
  DROP COLUMN IF EXISTS deployment_scope;

-- +goose StatementEnd
