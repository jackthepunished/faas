-- filename: 00277_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
--
-- 00277_reserve_slot.sql — reservation fence.
--
-- This slot is claimed by open PR #910 (feat(triggers): unified
-- event-source-mapping primitive — issue #757 / ADR-100) as a
-- reservation fence on origin/main. PR-D's cert engine branch
-- was forked from #937 before the 00277 fence landed; PR-D's
-- real migration renumbered to 00284 to step above the slots
-- PR #910 occupies. The 00277-280 fences land on PR-D to keep
-- TestMigrationsContiguous gap-free. Each file is a no-op; the
-- real migrations land when PR #910 merges (the real
-- migrations carry the same slot number and naturally replace
-- the fence on the merge).

select 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

select 1;

-- +goose StatementEnd
