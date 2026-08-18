-- filename: 00281_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
--
-- 00281_reserve_slot.sql — reservation fence.
--
-- This slot is claimed by open PR #910 (feat(triggers): unified
-- event-source-mapping primitive — issue #757 / ADR-100), which
-- introduces migrations/00281_triggers.sql on its own branch.
-- PR-D (issue #879 cert engine real-mint) renumbered its
-- migration to 00284 (above the highest slot PR #910 holds) but
-- needed fences here so TestMigrationsContiguous sees a gap-free
-- 281..284 sequence on PR-D's branch. The 00282/00283 fences
-- mirror the same fence shape. This file is a no-op; the actual
-- migration lands when PR #910 merges.

select 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

select 1;

-- +goose StatementEnd
