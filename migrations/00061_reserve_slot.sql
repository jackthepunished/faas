-- +goose Up
-- +goose StatementBegin

-- Slot 00061 reservation.
--
-- PR #403 (api: mutable per-app env vars, issue #395, ADR-045) holds
-- slot 61 on a separate branch and lands a real schema there. PR #399
-- (alerts: customer-configurable alert rules, issue #396 PR 1/4,
-- ADR-045) wants the next free slot after 61, which is 62. To keep
-- PR #399's embedded migration set contiguous 1..N through 62 without
-- conflicting with #403, this reservation holds 61 until PR #403
-- merges; once #403 lands at 61 with its real schema, this reservation
-- is dropped in a follow-up commit (per the same playbook PR #335 used
-- to drop the 00057 reservation after IAM-3 landed sessions at 57 —
-- see commit e243fb9e).
--
-- The cross-PR slot gate (scripts/ci/check_migration_slots.sh) sees
-- this file as a no-op reservation and does NOT count it as a slot
-- claim by PR #399; the local embed_test.go::TestMigrationsContiguous
-- mirrors that carve-out (ADR-041, PR #391 / PR #352).

select 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Slot 00061 reservation is forward-only by design; rolling it back
-- would re-expose the slot to the cross-PR gate and is not part of
-- any supported rollback path.

select 1;

-- +goose StatementEnd
