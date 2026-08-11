-- +goose Up
-- +goose StatementBegin

-- 00213_deployments_scope.sql — ADR-091 (Phase 3 of the
-- secrets+envs roadmap).
--
-- Wires per-deployment scope targeting onto the deployments
-- table. The companion `app_envs.scope` column landed in 00203
-- (ADR-090 PR-A), and the API surface landed in PR-B (#833,
-- merged 2026-08-11). What's missing is the deployment side:
-- today, every wake still merges `scope='default'` rows only
-- (loadAPIEnv at pkg/sched/engine.go::loadAPIEnv calls the
-- scope-agnostic `ListAppEnv`). ADR-091 closes the loop by
-- adding `scope` to deployments so schedd's wake can pick a
-- single scope's env rows per deployment.
--
-- Scope shape mirrors the existing `app_envs_scope_shape`
-- CHECK (00203) and `pkg/api/env_scope.go::EnvScopePattern`
-- — `^[a-z0-9]([a-z0-9-]{1,38})[a-z0-9]$`, lowercase alnum +
-- dash, 3..40 chars, no leading/trailing dash. We do NOT
-- invent a parallel regex; the Go-side helper ValidateScope
-- is reused verbatim on the create-deployment path.
--
-- Replay-safety: the constraint addition is guarded on
-- pg_catalog.pg_constraint so a second MigrateUp (e.g. after
-- a partial-apply where the goose row was lost) skips the
-- shape CHECK. Same convention as 00203 and 00064.
--
-- Cross-PR slot gate (revised five times): the original PR-D
-- draft landed at slot 00207, but two other open PRs (#826
-- obs-PR4 and #835 dashboard cron runs) also claimed 00207.
-- The cross-PR slot fence convention (memory/cross-pr-slot-
-- gate-fence-pattern) resolved "max(real_claims) + 1" → 00208.
-- After the first renumber, a third PR (#838) claimed 00208 —
-- so we renumber again to 00209 and add a reservation fence at
-- 00208 (held under ADR-041, carved out of the cross-PR-
-- collision check via slots_from_paths). After the second
-- renumber, a fourth PR (#829 paddle) claimed 00209 — so we
-- renumber once more to 00211 and add a reservation fence at
-- 00210 (held under ADR-041). After the third renumber, PR
-- #835's cron-uniqueness migration landed at slot 00210 on
-- main and our 00210 fence became a real-migration collision;
-- the rebase dropped that fence and renumbered to 00212. After
-- the fourth renumber, PR #838 (which had been on the same
-- slot-collision cluster as PR-D and was also chased through
-- the same renumber chain) won slot 00212 on its own merge
-- with 00212_github_webhook_secrets.sql, leaving our 00212 as a
-- duplicate. A second rebase dropped our 00212 file and
-- renumbered once more to 00213 — the next free slot on main.
-- Note: the 00209 slot is ALSO held by an ADR-041 fence
-- (migrations/00209_reserve_slot.sql) because
-- TestMigrationsContiguous uses position index, not just prefix —
-- every position N in the embedded set must have a NNNNN-prefix
-- file. Our real migration moved off 00209 to 00213, but a gap
-- at position 209 in the embedded set would fail
-- TestMigrationsContiguous ("migration slot 209 is missing").
-- The 00209 fence fills the slot. Slot map after PR-D lands:
-- 00200..00213 contiguous with the 00207, 00208, and 00209
-- fences holding the contested slots; slot 00210 is real
-- (#835's crons_unique_app_schedule_path migration), slot
-- 00211 is a fence (#838 cluster), slot 00212 is real (#838's
-- github_webhook_secrets migration). The two stale 00204 +
-- 00205 PR-A fences STAY on this branch —
-- TestMigrationsContiguous is strict ("never skip a slot"),
-- and PR-D does not consume those slots. A future PR that
-- lands a real migration at 00204, 00205, 00207, 00208, OR
-- 00209 drops the matching fence in its own commit.

ALTER TABLE deployments
    ADD COLUMN IF NOT EXISTS scope text NOT NULL DEFAULT 'default';

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conname = 'deployments_scope_shape'
          AND conrelid = 'deployments'::regclass
    ) THEN
        ALTER TABLE deployments
            ADD CONSTRAINT deployments_scope_shape
            CHECK (scope ~ '^[a-z0-9]([a-z0-9-]{1,38})[a-z0-9]$');
    END IF;
END$$;

-- The load-bearing invariant: at most one `live` deployment
-- per (app_id, scope). Without this index, two live
-- deployments on the same app with identical scope (e.g. two
-- `prod` deployments) would both pass `LiveDeployment(ctx,
-- appID)` and Postgres would pick one non-deterministically.
-- The partial predicate WHERE status='live' means the index
-- only covers live rows — superseded / failed / pending
-- deployments on the same (app_id, scope) are still allowed
-- (they have status != 'live' so they don't violate uniqueness).
-- A customer creating a second live deployment with the same
-- scope on one app receives 400 deployment_scope_collision
-- (mapped from Postgres 23505 / 23514-3).
CREATE UNIQUE INDEX IF NOT EXISTS deployments_app_scope_live_uniq
    ON deployments (app_id, scope)
    WHERE status = 'live';

-- Non-unique composite for the scope-aware read path. The
-- legacy `deployments_app_idx` (00001_init.sql:56) is kept
-- untouched — it still serves the account-scoped cascade.
CREATE INDEX IF NOT EXISTS deployments_app_scope_idx
    ON deployments (app_id, scope, created_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS deployments_app_scope_idx;
DROP INDEX IF EXISTS deployments_app_scope_live_uniq;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conname = 'deployments_scope_shape'
          AND conrelid = 'deployments'::regclass
    ) THEN
        ALTER TABLE deployments DROP CONSTRAINT deployments_scope_shape;
    END IF;
END$$;

ALTER TABLE deployments DROP COLUMN IF EXISTS scope;

-- +goose StatementEnd
