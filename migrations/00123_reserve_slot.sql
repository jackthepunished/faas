-- filename: 00123_reserve_slot.sql
-- Fence at slot 123 — held by this PR so the migration set stays
-- contiguous 1..N where N = 126 (this PR's highest). Bridges the
-- gap between 00122 (instances_framework_ready_at on main, real)
-- and 00124 (this PR's reserve_slot fence). PR #540 has its
-- 00123_webhook_deliveries.sql as a real schema on its branch;
-- when #540 lands, this fence shadows: the same PR drops this
-- file via `git rm` so the carved-out slot lands cleanly on main.
--
-- The cross-PR slot gate's reservation carve-out hides this
-- file from the collision check (slots_from_paths), so the
-- simultaneous reservations do not surface as a collision.
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