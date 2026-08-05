-- filename: 00142_reserve_slot.sql
-- Fence at slot 142 — held by PR #654 to bridge 00141 (own fence)
-- and 00143 (per-app require_authn). PR #653 owns 142 as a real
-- schema (sessions_binding); per ADR-041 the real schema shadows
-- this fence on whichever side merges first. Body is a no-op
-- `select 1;` so goose applies cleanly and writes a row in
-- goose_db_version.

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
