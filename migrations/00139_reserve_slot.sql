-- filename: 00139_reserve_slot.sql
-- Fence at slot 139 — held by PR #654 to bridge 00138 (PR #647
-- apps_eviction_priority) and 00143 (per-app require_authn). PR
-- #651 (issue #464 mega-PR) also claimed slot 139 with a real
-- schema; per ADR-041 the bridge fence gets `git rm`'d on whichever
-- side merges second — whoever's real schema lands at 139 first
-- shadows this fence. Body is a no-op `select 1;` so goose applies
-- cleanly and writes a row in goose_db_version.

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
