-- 00347_reserve_slot.sql — temporary concurrent-PR migration fence.
-- The real migration for this slot is being coordinated by other
-- open PRs (PR #1017 alert_presets catalog owns slots 00352-00356;
-- PR #990 ADR-117 PR-C owns slots 00350-00357 — see PR titles for
-- the exact owner). Remove this no-op when that migration lands,
-- per ADR-041.
-- +goose Up
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd