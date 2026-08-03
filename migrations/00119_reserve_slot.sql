-- filename: 00119_reserve_slot.sql
-- Fence at slot 119 — held by this PR so the migration set stays
-- contiguous (1..N where N = highest). The previous slot on main
-- is 00118 (deployments_sidecars, ADR-069); the next slot in this
-- PR is 00120 (warm_hint, ADR-070). Slot 119 itself is unowned —
-- no real schema lives here. ADR-041 (migration slot reservation
-- convention): the fence body is a no-op `select 1;` statement
-- inside goose's StatementBegin/End markers so goose applies it
-- cleanly and writes a row in goose_db_version. A later PR that
-- wants slot 119 for a real schema shadows this fence; the same
-- PR drops this file via `git rm` so the carved-out slot lands
-- cleanly on main.
--
-- This fence is required because main added fences at 00116 and
-- 00117 too; without this one the embedded set would have a gap
-- at slot 119 (TestMigrationsContiguous fails:
-- `migration slot 119 is missing`). The two-PR-collision history
-- (PRs #540 rebase moved its webhook schema to slot 119, #543
-- reserved 119/120/121 around its framework_ready at 122) means
-- the only way to land real schemas at 120+ is to also fence the
-- gap slots.

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
