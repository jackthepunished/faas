-- +goose Up
-- +goose StatementBegin

-- 00215_app_secrets_scope.sql — ADR-092 / Phase 4 of the
-- secrets+envs roadmap. Lifts the explicit ADR-090 D7 deferral
-- ("No sealed secrets per scope in Phase 2 — per-scope sealed
-- secrets ... deferred to a future ADR"). The goal: a `prod`
-- deployment can carry a sealed `DATABASE_URL` independently from
-- a `staging` deployment on the same app.
--
-- Widen the app_secrets PRIMARY KEY from (app_id, key) to
-- (app_id, scope, key) by adding a new `scope` column. The
-- default backfill is the PG11+ fast-default (same pattern as
-- 00203_app_envs_scope.sql:65-66 and
-- 00213_deployments_scope.sql:69-70): every pre-00215 row gets
-- scope='default' lazily on first read/write without an UPDATE
-- rewrite, so the migration is metadata-only on PG15.
--
-- Companion pieces of the same widening:
--   - migrations/00203_app_envs_scope.sql (env vars, shipped)
--   - migrations/00213_deployments_scope.sql (deployment scope,
--     shipped at slot 00213)
--   - this migration (sealed secrets, slot 00215)
--
-- Scope shape mirrors the existing `app_envs_scope_shape` CHECK
-- (00203) and `deployments_scope_shape` CHECK (00213) and
-- `pkg/api/env_scope.go::EnvScopePattern` — `^[a-z0-9]([a-z0-9-]{1,38})[a-z0-9]$`,
-- lowercase alnum + dash, 3..40 chars, no leading/trailing dash.
-- We do NOT invent a parallel regex; the Go-side helper
-- ValidateScope is reused verbatim on the secret handler path
-- (cmd/apid/handlers_secrets.go, PR-B).
--
-- Replay-safety: the PK-widening DO block is guarded on
-- `array_length(conkey, 1) = 2` so a second MigrateUp (e.g. after
-- a partial-apply where the goose row was lost) finds the new
-- 3-column PK and skips the drop+recreate. Same convention as
-- 00203 L85-97 and 00064_invocations_dead_letter.sql:56-71.
--
-- Cross-PR slot gate: PR-A claims slot 00215 — the next free
-- slot on main after the ADR-091 PR-D (`00213_deployments_scope`)
-- landed. No sibling PRs are currently contesting this slot.
-- Pre-merge verify with
--   git ls-tree origin/main migrations/ | grep '^00215'
-- must return zero matches before push.
--
-- The kid column on app_secrets (added by
-- 00191_app_secrets_kid.sql, ADR-089 PR-A) is left untouched —
-- the kid is the recipient identity that sealed the row, which is
-- independent of scope (sealing is per-app-per-key, scope only
-- changes which app_envs-style row we address). The
-- `app_secrets_kid_idx` (00191) on `kid WHERE kid IS NOT NULL` is
-- also kept untouched; the rekey walk uses kid as its cursor and
-- continues to work after the PK widening.
--
-- Index strategy: the new composite index
-- `app_secrets_account_app_scope_idx (account_id, app_id, scope)`
-- supports the scope-aware list path (PR-A's
-- `ListAppSecretsInScope`, the wake-time hot path called from
-- `pkg/sched/engine.go::loadSealedEnvFor`). The legacy
-- single-column indexes `app_secrets_account_idx`, `app_secrets_app_idx`,
-- the partial `app_secrets_org_id_idx` (00099), and the partial
-- `app_secrets_kid_idx` (00191) are all kept untouched — they
-- still serve the account-delete cascade, the org-membership
-- pruning path, and the rekey walk's kid-target query.

ALTER TABLE app_secrets
    ADD COLUMN IF NOT EXISTS scope text NOT NULL DEFAULT 'default';

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conname = 'app_secrets_scope_shape'
          AND conrelid = 'app_secrets'::regclass
    ) THEN
        ALTER TABLE app_secrets
            ADD CONSTRAINT app_secrets_scope_shape
            CHECK (scope ~ '^[a-z0-9]([a-z0-9-]{1,38})[a-z0-9]$');
    END IF;
END$$;

-- Widen the PK from (app_id, key) to (app_id, scope, key). The
-- `array_length(conkey, 1) = 2` guard is the replay-safety pin: a
-- second MigrateUp finds the new 3-column PK and skips both the
-- drop and the recreate. Mirrors the convention from
-- 00203_app_envs_scope.sql:85-97 and
-- 00064_invocations_dead_letter.sql:56-71.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conname = 'app_secrets_pkey'
          AND conrelid = 'app_secrets'::regclass
          AND array_length(conkey, 1) = 2
    ) THEN
        ALTER TABLE app_secrets DROP CONSTRAINT app_secrets_pkey;
        ALTER TABLE app_secrets
            ADD CONSTRAINT app_secrets_pkey PRIMARY KEY (app_id, scope, key);
    END IF;
END$$;

CREATE INDEX IF NOT EXISTS app_secrets_account_app_scope_idx
    ON app_secrets (account_id, app_id, scope);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS app_secrets_account_app_scope_idx;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conname = 'app_secrets_pkey'
          AND conrelid = 'app_secrets'::regclass
          AND array_length(conkey, 1) = 3
    ) THEN
        ALTER TABLE app_secrets DROP CONSTRAINT app_secrets_pkey;
        ALTER TABLE app_secrets
            ADD CONSTRAINT app_secrets_pkey PRIMARY KEY (app_id, key);
    END IF;
END$$;

ALTER TABLE app_secrets DROP CONSTRAINT IF EXISTS app_secrets_scope_shape;
ALTER TABLE app_secrets DROP COLUMN IF EXISTS scope;

-- +goose StatementEnd
