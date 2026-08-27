-- filename: 00487_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
--
-- Slot fence (ADR-041). The real migration lands at
-- 00488_deployment_scope_exclusions.sql (PR-B of the ADR-124
-- follow-up cluster). This fence exists so that any concurrent
-- PR claiming 00487 fails the migrations-check cross-PR slot
-- precheck (see scripts/ci/migrations-check.sh + the recent
-- slot-collision cascade in PR #1064 / PR #1049).
--
-- BRANCH NOTE: this branch (worktree-feat-affected-workload-
-- preview) was cut from origin/main commit `0b4cf07f4` where
-- the migration tail was 00386_mirror_invocation_results.sql.
-- Since then origin/main advanced many commits; the original
-- fence picked 00417 + 00418 but those slots were claimed by
-- PR #1070 alert-presets / alert-presets-seed after the rebase.
-- Renumbered to 00487 + 00488 against the CURRENT origin/main
-- tail (00486_events_operator_intents_trace_id.sql).

SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd