-- filename: 00155_reserve_slot.sql
-- Bridge fence at slot 155 — fills the gap between main's
-- 00154_deployment_liveness_probe and PR #698's
-- 00156_apps_auth_default_flip (issue #695 auth default flip). PR #697
-- originally claimed 155 for its parked_reason migration; renumbered
-- to 157 after PR #698 surfaced. ADR-041 carve-out: drop on rebase
-- once whichever PR lands second shadows this fence with a real
-- schema.

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