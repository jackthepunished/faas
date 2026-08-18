-- filename: 00283_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
--
-- 00283_reserve_slot.sql — reservation fence.
--
-- This slot is claimed by open PR #910 (feat(triggers): unified
-- event-source-mapping primitive — issue #757 / ADR-100), which
-- introduces migrations/00283_triggers_poison_strategy.sql. See
-- 00281_reserve_slot.sql for the cross-PR slot precheck pattern.
-- This file is a no-op; the actual migration lands when PR #910
-- merges.

select 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

select 1;

-- +goose StatementEnd
