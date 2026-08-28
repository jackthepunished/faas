-- filename: 00493_reserve_slot.sql
-- ADR-041 fence migration. Fills the slot to keep TestMigrationsContiguous
-- green on this branch (PR #1138 / issue #72 / ADR-133 PR-A3); the
-- cross-PR slot gate (scripts/ci/check_migration_slots.sh) treats
-- _reserve_slot.sql as a non-claim so siblings can renumber onto this
-- slot if needed. Filled the gap after main advanced to 00487 and the
-- renumber chain collided with PR #1024's 00491-00493 + PR #1123's
-- 00487-00488.
-- +goose Up
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
