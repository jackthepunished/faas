-- 00358_reserve_slot.sql — temporary concurrent-PR migration fence.
-- The real migration for this slot is being coordinated by other
-- open PRs (PR #1005 api-contract-diff PR-A owns 00358 as a real
-- migration deployment_openapi_snapshots). Remove this no-op when
-- that migration lands, per ADR-041.
-- +goose Up
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd