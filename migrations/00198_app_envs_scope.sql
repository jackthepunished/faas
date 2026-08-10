-- +goose Up
-- +goose StatementBegin

-- 00198_app_envs_scope.sql — ADR-090 PR-A.
--
-- Widen the app_envs PRIMARY KEY from (app_id, key) to (app_id, scope,
-- key) by adding a new `scope` column. The default backfill is the
-- PG11+ fast-default — see ADR-090 D1. Every pre-00198 row gets
-- scope='default' lazily on first read/write without an UPDATE
-- rewrite, so the migration is metadata-only on PG15.
--
-- Scope shape mirrors the server-side validSlug() regex at
-- cmd/apid/handlers.go:600 — `^[a-z0-9]([a-z0-9-]{1,38})[a-z0-9]$`
-- (lowercase alnum + dash, 3..40 chars, no leading/trailing dash).
-- A scope is NOT a free-form string: it's a domain-valid slug that
-- the operator can reference from `gregale env set --scope staging
-- KEY=val` (PR-B) and from the `/v1/apps/{slug}/envs?scope=` API
-- (PR-B). The shape is intentionally the same as `apps.slug` so
-- scope-based env overrides can be addressed by the same identifier
-- the operator already uses for app slugs.
--
-- Replay-safety: the PK-widening DO block is guarded on
-- `array_length(conkey, 1) = 2` so a second MigrateUp (e.g. after a
-- partial-apply where the goose row was lost) finds the new 3-column
-- PK and skips the drop+recreate. The shape is the convention from
-- 00064_invocations_dead_letter.sql:56-71 and
-- 00053_deployments_source_url.sql:57-70.
--
-- Cross-PR slot gate: 00198 is collision-clean at PR-A open time
-- (main's max real migration is 00197_builds_deployment_started_idx;
-- 00193/00195/00196 are pre-existing fences unrelated to ADR-090).
-- Re-verify `git ls-tree origin/main migrations/ | grep 00198` is
-- empty immediately before commit. If a competing PR lands 00198
-- first, renumber PR-A to the next free slot and amend the fence
-- removal accordingly.
--
-- Index strategy: the new composite index
-- `app_envs_account_app_scope_idx (account_id, app_id, scope)`
-- supports the scope-aware list path (PR-A's
-- `ListAppEnvInScope`). The legacy single-column indexes
-- `app_envs_account_idx`, `app_envs_app_idx`, and the partial
-- `app_envs_org_id_idx` are kept untouched — they still serve the
-- account-delete cascade at pgstore.go:11897-11900 and the
-- org-membership pruning path. PR-C may revisit whether to drop
-- them once the new access pattern is observable in production.

ALTER TABLE app_envs
    ADD COLUMN IF NOT EXISTS scope text NOT NULL DEFAULT 'default';

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conname = 'app_envs_scope_shape'
          AND conrelid = 'app_envs'::regclass
    ) THEN
        ALTER TABLE app_envs
            ADD CONSTRAINT app_envs_scope_shape
            CHECK (scope ~ '^[a-z0-9]([a-z0-9-]{1,38})[a-z0-9]$');
    END IF;
END$$;

-- Widen the PK from (app_id, key) to (app_id, scope, key). The
-- `array_length(conkey, 1) = 2` guard is the replay-safety pin: a
-- second MigrateUp finds the new 3-column PK and skips both the
-- drop and the recreate. Mirrors the convention from
-- 00064_invocations_dead_letter.sql:56-71.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conname = 'app_envs_pkey'
          AND conrelid = 'app_envs'::regclass
          AND array_length(conkey, 1) = 2
    ) THEN
        ALTER TABLE app_envs DROP CONSTRAINT app_envs_pkey;
        ALTER TABLE app_envs
            ADD CONSTRAINT app_envs_pkey PRIMARY KEY (app_id, scope, key);
    END IF;
END$$;

CREATE INDEX IF NOT EXISTS app_envs_account_app_scope_idx
    ON app_envs (account_id, app_id, scope);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS app_envs_account_app_scope_idx;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conname = 'app_envs_pkey'
          AND conrelid = 'app_envs'::regclass
          AND array_length(conkey, 1) = 3
    ) THEN
        ALTER TABLE app_envs DROP CONSTRAINT app_envs_pkey;
        ALTER TABLE app_envs
            ADD CONSTRAINT app_envs_pkey PRIMARY KEY (app_id, key);
    END IF;
END$$;

ALTER TABLE app_envs DROP CONSTRAINT IF EXISTS app_envs_scope_shape;
ALTER TABLE app_envs DROP COLUMN IF EXISTS scope;

-- +goose StatementEnd
