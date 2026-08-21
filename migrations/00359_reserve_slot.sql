-- 00359_reserve_slot.sql — temporary concurrent-PR migration fence.
-- The real migration for this slot is being coordinated by other
-- open PRs (PR #1006 SAFE-RELEASES deployment_audit owns 00359 as
-- a reservation; real migrations at 00360-00361). Remove this
-- no-op when that migration lands, per ADR-041.
-- +goose Up
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd