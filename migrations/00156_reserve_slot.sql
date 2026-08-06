-- filename: 00156_reserve_slot.sql
-- Bridge fence at slot 156 — fills the gap between PR #698's
-- 00155_reserve_slot and PR #697's 00157_deployments_parked_reason
-- (issue #554 / ADR-079 follow-up). PR #698 claims 156 for its
-- apps_auth_default_flip migration; PR #697 renumbered past it to 157.
-- ADR-041 carve-out: drop on rebase once whichever PR lands second
-- shadows this fence with a real schema.

-- +goose Up
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
-- +goose StatementBegin
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
-- +goose StatementBegin
-- +goose StatementEnd