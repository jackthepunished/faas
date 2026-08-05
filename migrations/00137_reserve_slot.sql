-- filename: 00137_reserve_slot.sql
-- Fence at slot 137 — held by PR #654 to bridge 00136 and 00143
-- after the cross-PR slot cascade. PR #651's real schema
-- (deployments_scan_result) holds 137 elsewhere; this fence is
-- meta — when the collision settles on main, whoever's real
-- schema lands at 137 shadows this fence via `git rm`. Per
-- ADR-041 the body is a no-op SELECT 1.

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
