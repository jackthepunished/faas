-- filename: 00121_reserve_slot.sql
-- Fence at slot 121 — held by this PR so the migration set stays
-- contiguous 1..N where N = 124 (this PR's highest). Main dropped
-- its own 00121_reserve_slot.sql when this branch's 00121_pg_ratelimit
-- landed on the branch; without a replacement fence the embedded
-- set has a gap at 121 (TestMigrationsContiguous fails:
-- `migration slot 121 is missing`).
--
-- Note: this slot is also contested with PR #552, which has a real
-- schema (events_sidecar_name_idx) at 121. Whichever side lands
-- first, the other's 121 fence drops on rebase. The cross-PR slot
-- gate hides reservation files via the slots_from_paths regex
-- carve-out, so simultaneous reservations do not surface as a
-- collision.
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