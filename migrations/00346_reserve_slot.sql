-- 00346_reserve_slot.sql — temporary concurrent-PR migration fence.
-- The real migration for this slot is claimed by another open PR.
-- Remove this no-op when that migration lands, per ADR-041.
-- +goose Up
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd