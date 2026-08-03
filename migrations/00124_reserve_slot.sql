-- filename: 00124_reserve_slot.sql
-- Fence at slot 124 — held by this PR so the migration set stays
-- contiguous 1..N where N = 126 (this PR's highest). Bridges the
-- gap between PR #540's 00123_webhook_deliveries.sql (real) and
-- this PR's 00125_warm_hint.sql (real). Without this fence the
-- embedded set has a gap at 124 (TestMigrationsContiguous fails:
-- `migration slot 124 is missing`).
--
-- ADR-041 (migration slot reservation convention): the fence body
-- is a no-op `select 1;` inside goose's StatementBegin/End markers
-- so goose applies it cleanly and writes a row in goose_db_version.

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