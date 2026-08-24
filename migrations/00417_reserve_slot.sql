-- filename: 00417_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
--
-- Slot fence (ADR-041). The real migration lands at
-- 00418_deployment_scope_exclusions.sql (PR-B of the ADR-124
-- follow-up cluster). This fence exists so that any concurrent
-- PR claiming 00417 fails the migrations-check cross-PR slot
-- precheck (see scripts/ci/migrations-check.sh + the recent
-- slot-collision cascade in PR #1064 / PR #1049).
--
-- BRANCH NOTE: this branch (worktree-feat-affected-workload-
-- preview) was cut from origin/main commit `0b4cf07f4` where
-- the migration tail was 00386_mirror_invocation_results.sql.
-- Since then origin/main advanced 140 commits and the tail is
-- now 00416_openapi_import.sql (PR #1049 merged). Picking
-- 00417 here is correct against the CURRENT origin/main head;
-- the author MUST rebase this PR onto current origin/main
-- before opening — `git fetch origin && git rebase
-- origin/main` and verify no other PR claims 00417 or 00418
-- in the cross-PR slot precheck output. If a conflict is
-- found, renumber to 00419/00420 (or beyond) following the
-- PR #1024 / #1064 / #1070 renumber pattern.

SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd