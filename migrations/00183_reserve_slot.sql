-- +goose Up
-- +goose StatementBegin
--
-- 00183_reserve_slot.sql — slot reservation placeholder
-- (ADR-041 / PR #391 migration gate carve-out).
--
-- This fence is owned by PR #805 (operator observability backend,
-- issue #777 / ADR-091). Slots 00174-00189 are intentionally
-- skipped between 00173_invocations_outcome.sql and 00190_admin_obs_index.sql: the pre-merge cross-PR gate
-- flagged slots in the 168-172 band (PRs #797 / #800 claim them)
-- and 173 = invocations_outcome is already merged. The PR #805
-- fence chain (174..00189) lets PR #805 take slot 00190_admin_obs_index.sql without
-- disturbing the existing reservations.
--
-- A future rebase that drops these fences before PR #805 merges is
-- allowed but not expected — the expected merge order is #797,
-- #800, then #805. The fences are removed as part of PR #805's
-- follow-up slice (a slot-roll-back PR) once the queue drains.
--
-- Body: `select 1;` — executes against the live DB at apply time
-- but produces no schema change.
--
select 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
select 1;
-- +goose StatementEnd
