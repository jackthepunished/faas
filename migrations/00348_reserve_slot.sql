-- 00348_reserve_slot.sql — temporary concurrent-PR migration fence.
-- PR #1006 (SAFE-RELEASES deployment_audit) and PR #1017 (alert
-- presets/extensions) both claim real slot 00348. PR #1012
-- (stages-prod-ready) cannot merge a real migration at 00348
-- without colliding when either #1006 or #1017 merges second, so
-- this fence fills the local contiguity gap until those PRs land.
-- Remove this no-op when that migration lands, per ADR-041.
-- +goose Up
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd