-- filename: 00289_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
--
-- 00289_reserve_slot.sql — reservation fence.
--
-- This slot is claimed by open PR #910 (feat(triggers): unified
-- event-source-mapping primitive — issue #757 / ADR-100), which
-- introduces migrations/00289_*.sql on its own branch (or
-- whatever real schema ultimately lands at slot 89 on PR #910 —
-- see PR #910's migration set for the canonical list). The
-- error-explanations cluster (spec §6.4 amendment 1, ADR-110
-- amendment 1) renumbered its migration to 00290 (above PR #910's
-- 00288 + 00289) but needs fences here so TestMigrationsContiguous
-- sees a gap-free 289..290 sequence on this branch. This file is a
-- no-op; the actual migration lands when PR #910 merges.

select 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

select 1;

-- +goose StatementEnd
