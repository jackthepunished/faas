-- +goose Up
-- +goose StatementBegin
-- filename: 00288_reserve_slot.sql
--
-- Slot reservation fence for issue #977 / ADR-116 (deployment annotations).
-- Body is the verbatim `select 1;` no-op per scripts/ci/check_migration_slots.sh
-- and migrations/00285_reserve_slot.sql. Converts to a real migration when
-- the mega-PR ships; the apply-walk test still counts this slot toward
-- TestMigrationsContiguous so the sequence 1..N stays gap-free.
select 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
select 1;
-- +goose StatementEnd
