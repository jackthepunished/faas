-- filename: 00480_deployments_canary_state.sql
-- +goose Up
-- +goose StatementBegin

-- Issue #976 / ADR-122 (SAFE-RELEASES-A + F) — canary presets and the
-- rollout state machine that A's canary_progression meterd tick and F's
-- safe-deploy orchestrator walk. The columns are stamped at deploy time
-- (apid, BuildDeploymentForInsert) and mutated by meterd ticks; the
-- apid REST surface never writes them directly (preserves
-- "apid = only writer to deployments" ownership from CLAUDE.md — meterd
-- is a downstream state-machine driver, not a customer-intent writer,
-- and all meterd writes still go through apid's authoritative endpoints
-- PatchDeploymentsIdTraffic + RollbackTo via pkg/api.Client).
--
-- canary_preset is a closed-set of catalog names from
-- pkg/api/canary/preset.go (none/slow/balanced/aggressive/1-10-50-100).
-- The CLI default for `--canary-preset 1-10-50-100` is the alias of
-- `balanced`; the closed set on disk is the 5 catalog names.
--
-- canary_step / canary_total_steps: bounded integers; total=0 means
-- "no canary preset applied" (the deployment's traffic is just 100%
-- on the new row, with no staged ladder). The canary_step_bounds_chk
-- CHECK below locks this invariant in the schema so a stray
-- meterd-side bug can't drift into a step beyond total.
--
-- rollout_state is the state machine: pending → rolling_out → complete,
-- with aborted reachable from pending/rolling_out (rollback path). The
-- orchestrator (commit 5) walks this state every 30 s; the apid Create
-- path stamps 'pending' at INSERT; transitions happen only on meterd
-- ticks or via the manual `gregale rollouts recover` CLI (commit 6).
--
-- Back-compat: every column is NOT NULL DEFAULT <zero-value>. PG11+
-- fast-default makes this metadata-only on pre-PR rows; existing
-- pre-PR deployments get (none,0,0,null,pending,null,null,null,null)
-- at the next catalog read with no row rewrite.
--
-- canary_step_started_at / rollout_started_at / rollout_completed_at /
-- rollout_aborted_at are nullable because pre-PR rows + pre-step-1
-- deployments have no meaningful "since when" timestamp. The orchestrator
-- uses NULL-on-pending as its "not yet ticking" sentinel.

ALTER TABLE deployments
    ADD COLUMN IF NOT EXISTS canary_preset text NOT NULL DEFAULT 'none',
    ADD COLUMN IF NOT EXISTS canary_step integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS canary_total_steps integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS canary_step_started_at timestamptz,
    ADD COLUMN IF NOT EXISTS rollout_state text NOT NULL DEFAULT 'pending',
    ADD COLUMN IF NOT EXISTS rollout_started_at timestamptz,
    ADD COLUMN IF NOT EXISTS rollout_completed_at timestamptz,
    ADD COLUMN IF NOT EXISTS rollout_aborted_at timestamptz,
    ADD COLUMN IF NOT EXISTS rollout_aborted_reason text;

ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_canary_preset_chk;
ALTER TABLE deployments
    ADD CONSTRAINT deployments_canary_preset_chk
        CHECK (canary_preset IN ('none','slow','balanced','aggressive','1-10-50-100'));

ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_rollout_state_chk;
ALTER TABLE deployments
    ADD CONSTRAINT deployments_rollout_state_chk
        CHECK (rollout_state IN ('pending','rolling_out','complete','aborted'));

-- canary_step_bounds_chk: either the deployment has no canary preset
-- (total=0, step=0 — the fast-default zero-value), OR it has a canary
-- preset (total>0) and step is in [0, total]. This stops a buggy
-- canary_progression tick from overshooting the ladder into step=5
-- when total=3.
ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_canary_step_bounds_chk;
ALTER TABLE deployments
    ADD CONSTRAINT deployments_canary_step_bounds_chk
        CHECK (
            (canary_total_steps = 0 AND canary_step = 0)
            OR
            (canary_total_steps > 0 AND canary_step >= 0 AND canary_step <= canary_total_steps)
        );

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_canary_step_bounds_chk;
ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_rollout_state_chk;
ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_canary_preset_chk;
ALTER TABLE deployments
    DROP COLUMN IF EXISTS rollout_aborted_reason,
    DROP COLUMN IF EXISTS rollout_aborted_at,
    DROP COLUMN IF EXISTS rollout_completed_at,
    DROP COLUMN IF EXISTS rollout_started_at,
    DROP COLUMN IF EXISTS rollout_state,
    DROP COLUMN IF EXISTS canary_step_started_at,
    DROP COLUMN IF EXISTS canary_total_steps,
    DROP COLUMN IF EXISTS canary_step,
    DROP COLUMN IF EXISTS canary_preset;

-- +goose StatementEnd
