-- filename: 00130_reserve_slot.sql
-- Fence at slot 130 — held by this PR so the migration set stays
-- contiguous 1..N where N = 132 (this PR's highest). Bridges the
-- gap between this PR's 00129_reserve_slot.sql (fence) and
-- 00131_apps_align_min_instances.sql (real, renamed from 00129 due
-- to PR #623's slot 129 claim). Without this fence the embedded
-- set has a gap at 130 (TestMigrationsContiguous fails:
-- `migration slot 130 is missing`).
--
-- ADR-041 (migration slot reservation convention): the fence body
-- is a no-op `select 1;` inside goose's StatementBegin/End markers
-- so goose applies it cleanly and writes a row in goose_db_version.
-- A later PR that wants slot 130 for a real schema shadows this
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