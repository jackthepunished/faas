-- filename: 00296_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
--
-- 00296_reserve_slot.sql — reservation fence.
--
-- Claimed by the deploy-stage-progress PR (branch
-- worktree-feat-deploy-stage-progress, ADR-117). The real migration
-- migrations/00296_deployments_stage_state.sql lands on this branch
-- in the same PR. This file is a no-op; the test
-- migrations/00296_deployments_stage_state_test.go applies through
-- 00296 on the branch tip.

select 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

select 1;

-- +goose StatementEnd
