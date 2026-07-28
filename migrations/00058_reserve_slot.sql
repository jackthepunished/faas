-- +goose Up
-- +goose StatementBegin

-- Slot 00058 reservation.
--
-- PR #369 (billing: credit consumption reducer, issue #279) holds
-- slot 58 on a separate branch and lands a real schema there. PR-C
-- (#352, githubd durable install state) wants the next free slot
-- after 58, which is 59. To keep PR-C's embedded migration set
-- contiguous 1..N through 59 without conflicting with #369, this
-- reservation holds 58 until PR #369 merges; once #369 lands at 58
-- with its real schema, this reservation is dropped in a follow-up
-- commit (per the same playbook PR #335 used to drop the 00057
-- reservation after IAM-3 landed sessions at 57 — see commit
-- e243fb9e).
--
-- The cross-PR slot gate (scripts/ci/check_migration_slots.sh) sees
-- this file as a no-op reservation and does NOT count it as a slot
-- claim by PR-C; the local embed_test.go::TestMigrationsContiguous
-- mirrors that carve-out (ADR-041, PR #391 / PR #352).

select 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Slot 00058 reservation is forward-only by design; rolling it back
-- would re-expose the slot to the cross-PR gate and is not part of
-- any supported rollback path.

select 1;

-- +goose StatementEnd