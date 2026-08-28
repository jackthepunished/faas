-- filename: 00484_reserve_slot.sql
-- The canary-state migration was moved to 00485 to avoid a cross-PR
-- collision. Keep the append-only Goose history contiguous at 00484.
-- +goose Up
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
