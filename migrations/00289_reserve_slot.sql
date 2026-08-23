-- filename: 00289_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin

-- 00289_reserve_slot.sql — reservation fence (see 00288 for the
-- full explanation). Same purpose, one slot higher.

SELECT 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

SELECT 1;

-- +goose StatementEnd