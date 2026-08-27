-- filename: 00481_reserve_slot.sql
-- The capacity index was moved to 00482 after the original 00481 slot
-- collided with another migration. Keep the goose history contiguous.
-- +goose Up
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
