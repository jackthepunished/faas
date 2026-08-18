-- filename: 00279_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
--
-- 00279_reserve_slot.sql — reservation fence.
--
-- Companion to 00277. Claimed by PR #910 on origin/main;
-- mirrored here on PR-D's branch so TestMigrationsContiguous
-- sees a gap-free sequence from 277..284.

select 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

select 1;

-- +goose StatementEnd
