-- +goose Up
-- +goose StatementBegin
--
-- 00057_reserve_slot.sql — slot reservation placeholder
-- (ADR-041 / PR #391 migration gate carve-out).
--
-- Pair to 00056_reserve_slot.sql; same shape, same rationale.
-- PR #369 (issue #279 PR-C, credit consumption reducer) sits at
-- slot 58; slots 56 and 57 are reserved as no-ops until open PRs
-- #335 and #352 land and overwrite them with their real schemas.
--
-- See 00056_reserve_slot.sql for the full comment block.
--
select 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- No-op: nothing to reverse.
-- +goose StatementEnd