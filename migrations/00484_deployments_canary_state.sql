-- filename: 00484_deployments_canary_state.sql
-- +goose Up
-- +goose StatementBegin

-- SAFE-RELEASES (issue #976 / ADR-122): durable canary and rollout
-- state used by the meterd orchestrator and operator recovery path.
-- Defaults preserve the pre-canary single-deployment behavior.
ALTER TABLE deployments
    ADD COLUMN IF NOT EXISTS canary_preset TEXT NOT NULL DEFAULT 'none',
    ADD COLUMN IF NOT EXISTS canary_step INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS canary_total_steps INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS canary_step_started_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS rollout_state TEXT NOT NULL DEFAULT 'pending',
    ADD COLUMN IF NOT EXISTS rollout_started_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS rollout_completed_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS rollout_aborted_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS rollout_aborted_reason TEXT;

ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_canary_preset_chk,
    DROP CONSTRAINT IF EXISTS deployments_canary_step_nonneg_chk,
    DROP CONSTRAINT IF EXISTS deployments_canary_total_steps_nonneg_chk,
    DROP CONSTRAINT IF EXISTS deployments_rollout_state_chk;

ALTER TABLE deployments
    ADD CONSTRAINT deployments_canary_preset_chk
        CHECK (canary_preset IN ('none', 'slow', 'balanced', 'aggressive', '1-10-50-100')),
    ADD CONSTRAINT deployments_canary_step_nonneg_chk
        CHECK (canary_step >= 0),
    ADD CONSTRAINT deployments_canary_total_steps_nonneg_chk
        CHECK (canary_total_steps >= 0),
    ADD CONSTRAINT deployments_rollout_state_chk
        CHECK (rollout_state IN ('pending', 'rolling_out', 'complete', 'aborted'));

CREATE INDEX IF NOT EXISTS deployments_canary_inflight_idx
    ON deployments (status, canary_total_steps, canary_step)
    WHERE status = 'live' AND canary_total_steps > 0;

CREATE INDEX IF NOT EXISTS deployments_rollout_pending_idx
    ON deployments (rollout_state, rollout_started_at, created_at)
    WHERE status = 'live' AND rollout_state IN ('pending', 'rolling_out');

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS deployments_rollout_pending_idx;
DROP INDEX IF EXISTS deployments_canary_inflight_idx;
ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_rollout_state_chk,
    DROP CONSTRAINT IF EXISTS deployments_canary_total_steps_nonneg_chk,
    DROP CONSTRAINT IF EXISTS deployments_canary_step_nonneg_chk,
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
