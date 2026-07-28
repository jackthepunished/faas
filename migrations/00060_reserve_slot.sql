-- +goose Up
-- +goose StatementBegin

-- Slot 00060 reservation.
--
-- PR #401 (queue: introspection endpoints + dead_letter state, issue #394)
-- holds slot 60 on a separate branch and lands a real schema there. PR #399
-- (alerts: customer-configurable alert rules, issue #396 PR 1/4, ADR-045)
-- wants the next free slot after 60, which is 61. To keep PR #399's
-- embedded migration set contiguous 1..N through 61 without conflicting
-- with #401, this reservation holds 60 until PR #401 merges; once #401
-- lands at 60 with its real schema, this reservation is dropped in a
-- follow-up commit (per the same playbook PR #335 used to drop the 00057
-- reservation after IAM-3 landed sessions at 57 — see commit e243fb9e).
--
-- The cross-PR slot gate (scripts/ci/check_migration_slots.sh) sees this
-- file as a no-op reservation and does NOT count it as a slot claim by
-- PR #399; the local embed_test.go::TestMigrationsContiguous mirrors that
-- carve-out (ADR-041, PR #391 / PR #352).

select 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Slot 00060 reservation is forward-only by design; rolling it back would
-- re-expose the slot to the cross-PR gate and is not part of any
-- supported rollback path.

select 1;

-- +goose StatementEnd
