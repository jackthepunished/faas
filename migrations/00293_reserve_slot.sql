-- filename: 00293_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin

-- 00293_reserve_slot.sql — reservation fence (see 00288 for the
-- full explanation). NOTE: PR #978 (issue #975 mega-foundation)
-- uses this slot for the real migration 00293_validate_mode.sql.
-- When both PRs land on main, this fence is replaced by PR #978's
-- real migration on main. Same purpose, one slot higher.

SELECT 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

SELECT 1;

-- +goose StatementEnd