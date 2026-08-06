-- filename: 00151_apps_public_auth.sql
-- +goose Up
-- Issue #477 / ADR-077 — per-app public-URL auth mode
-- ('open'|'bearer'|'basic') + sealed credential blob for the
-- basic-auth path. Adds two columns to apps; when mode='bearer'
-- or 'basic', gatewayd-internal demands the matching Authorization
-- header on every routed request. The default 'open' preserves the
-- pre-#477 customer behaviour — every existing app stays
-- public-by-default. Plan gates (open=all, bearer=Hobby+, basic=Pro+)
-- are enforced at the apid PATCH validator; the CHECK here is the
-- data-integrity backstop.
--
-- Two columns + one CHECK:
--
--   public_auth_mode   text NOT NULL DEFAULT 'open'
--                        CHECK (public_auth_mode IN ('open','bearer','basic'))
--   public_auth_basic  bytea           -- sealed via secretbox
--                                       -- namespace APP_BASIC_AUTH;
--                                       -- only set when mode='basic'.
--                                       -- Nullable: stays NULL for
--                                       -- open/bearer modes. The
--                                       -- secretbox payload carries
--                                       -- {username_env, password_env}
--                                       -- env-var reference names (the
--                                       -- issue body explicitly defers
--                                       -- plaintext creds at PATCH
--                                       -- time — creds live in
--                                       -- app_secrets per ADR-045).
--
-- Replay-safe (ADR-041): ADD COLUMN IF NOT EXISTS + DO-block
-- pg_catalog.pg_constraint guard for the CHECK. Postgres has IF NOT
-- EXISTS for ADD COLUMN but not for ADD CONSTRAINT; the DO-block
-- pattern matches migrations/00082_apps_scaling_policy.sql:50-77,
-- 00109_apps_warm_snapshot.sql:44-64, and 00074_projects_and_workloads
-- .sql:86-104. The 00138_apps_eviction_priority.sql:46-62 shape is
-- the closest precedent (text + enum CHECK).
--
-- Slot 151 — picked after the slot fence walk. Slot 149 is fenced
-- by PR #673 (issue #554 liveness probe) and is also touched by
-- PR #540 (issue #476 webhook deliveries follow-up). Slot 150 is
-- PR #673's real migration. 151 is the next uncontested slot
-- above the active range. Per ADR-041 + memory
-- cross-pr-slot-gate-races-with-active-pr, the rule is: pick the
-- lowest slot above the contested range, and keep the renumber
-- chain in this paragraph so a future rebase reader sees the
-- full timeline. If main catches up on its own (e.g. #673 or
-- #540 lands first, vacating 149/150), drop this paragraph but
-- keep the filename + filename header consistent.
-- +goose StatementBegin

ALTER TABLE apps
    ADD COLUMN IF NOT EXISTS public_auth_mode text NOT NULL DEFAULT 'open';

ALTER TABLE apps
    ADD COLUMN IF NOT EXISTS public_auth_basic bytea;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conname = 'apps_public_auth_mode_chk'
          AND conrelid = 'apps'::regclass
    ) THEN
        ALTER TABLE apps
            ADD CONSTRAINT apps_public_auth_mode_chk
            CHECK (public_auth_mode IN ('open','bearer','basic'));
    END IF;
END$$;

-- +goose StatementEnd

-- +goose Down
-- Reverse: drop the CHECK first (so the column drop doesn't trip
-- the constraint), then drop both columns. A row that had
-- public_auth_mode='bearer' or 'basic' loses the config on
-- downgrade; the GET /v1/apps/{slug} response shape omits
-- public_auth because the columns no longer exist, which is the
-- correct degraded behaviour (open-by-default, same pattern as
-- migrations/00080_apps_streaming_enabled.sql::Down and
-- migrations/00109_apps_warm_snapshot.sql::Down). The sealed
-- public_auth_basic blob is dropped with the column.
-- +goose StatementBegin
ALTER TABLE apps
    DROP CONSTRAINT IF EXISTS apps_public_auth_mode_chk;

ALTER TABLE apps
    DROP COLUMN IF EXISTS public_auth_basic;

ALTER TABLE apps
    DROP COLUMN IF EXISTS public_auth_mode;
-- +goose StatementEnd
