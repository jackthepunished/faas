-- filename: 00288_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin

-- 00288_reserve_slot.sql — reservation fence.
--
-- This slot is fenced by issue #961 Mega-C PR-1 (preview
-- close-out). The Mega-C work needs a real migration at 00296
-- (apps.preview_destroy_commented_at — see migrations/00296).
-- Slots 00288–00295 are fenced on this branch so
-- TestMigrationsContiguous sees a gap-free sequence while PR #978
-- (issue #975 mega-foundation; kind=validate modes) still holds
-- those slots on its own branch. The cross-PR slot precheck
-- (memory: cross-pr-slot-precheck-pr-867-collision-2026-08-13)
-- scans origin/main + open-PR heads; when both PRs land, only
-- one copy of each fence survives on main and the no-op bodies
-- (`SELECT 1;`) are safe to drop in either order. The comment
-- block in THIS fence references issue #961 explicitly so the
-- cross-PR rebase-fence deletion hazard (memory:
-- cross-pr-rebase-fence-deletion-hazard) does not delete the
-- wrong copy on merge — fences with distinct comment blocks
-- survive a cherry-pick.

SELECT 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

SELECT 1;

-- +goose StatementEnd