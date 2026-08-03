-- filename: 00120_reserve_slot.sql
-- Fence at slot 120 — held by this PR so the migration set stays
-- contiguous 1..N where N = 124 (this PR's highest). Main dropped
-- its own 00120_reserve_slot.sql when this branch's 00120_warm_hint
-- landed on the branch; without a replacement fence the embedded
-- set has a gap at 120 (TestMigrationsContiguous fails:
-- `migration slot 120 is missing`).
--
-- ADR-041 (migration slot reservation convention): the fence body
-- is a no-op `select 1;` inside goose's StatementBegin/End markers
-- so goose applies it cleanly and writes a row in goose_db_version.
-- A later PR that wants slot 120 for a real schema shadows this
-- fence; the same PR drops this file via `git rm` so the carved-
-- out slot lands cleanly on main.

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