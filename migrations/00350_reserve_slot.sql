-- 00350_reserve_slot.sql — temporary concurrent-PR migration fence.
-- The real migration for this slot is being coordinated by other
-- open PRs (PR #990 ADR-117 PR-C app_secret_value_hash owns this
-- slot range through 00357; PR #1017 alert_presets catalog owns
-- slots 00352-00356). Remove this no-op when those migrations land,
-- per ADR-041.
-- +goose Up
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd