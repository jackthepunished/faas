# migrations/ — goose, numbered, append-only (spec §5)

Never edit a merged migration. Schema authored in spec §5; sqlc generates typed
queries against it. Every state column carries a CHECK constraint.

## Slot reservations

If your PR needs to claim a migration slot but the schema isn't ready yet
(typical when multiple PRs are racing for the same next free slot — see
ADR-041), drop a no-op reservation at that slot:

```sql
-- +goose Up
-- +goose StatementBegin
select 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
select 1;
-- +goose StatementEnd
```

Filenames containing `_reservation` or `_reserve_slot` (case-insensitive)
are reservations. The cross-PR slot gate (`scripts/ci/check_migration_slots.sh`)
ignores them when computing overlaps, so multiple PRs can hold
reservations at the same slot while only their real schemas must not
collide. Canonical name: `NNNNN_reserve_slot.sql`. See ADR-041.

Reservations still count toward the embedded 1..N contiguity required by
`embed_test.go::TestMigrationsContiguous` — they are real migrations that
just don't do anything, and goose still applies them.
