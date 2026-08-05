-- filename: 00139_reserve_slot.sql
-- Fence at slot 139 — bridge between 00138 (PR #656 apps_eviction_priority)
-- and 00140 (PR #658 webhook claims). PR #656 originally held slot 139
-- with deployments_scan_result; the slot was renumbered to 00144 to
-- dodge the open-PR collision gate (PR #651 / issue #464 mega-PR also
-- claims 139). Per ADR-041 the bridge fence stays as a connector.
-- Body is a no-op `select 1;` so goose applies cleanly and writes a
-- row in goose_db_version.

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
