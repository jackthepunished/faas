-- 00350_reserve_slot.sql — temporary concurrent-PR migration fence.
-- PR #1017 (alert_presets/account_spend_snapshot) claims real slot
-- 00350. PR #1012 (stages-prod-ready) cannot merge a real migration
-- at 00350 without colliding when #1017 merges second, so this
-- fence fills the local contiguity gap until that migration
-- lands. Remove this no-op when that migration lands, per ADR-041.
-- +goose Up
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd