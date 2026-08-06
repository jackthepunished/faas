-- filename: 00149_reserve_slot.sql
-- Bridge fence at slot 149 — fills the gap between main's
-- 00148_overage_cap_gate_index and PR #671's 00150_wait_until_tail
-- (issue #667 / ADR-078). Slot 149 is also claimed by open PR
-- #540 (test(state): raise pkg/state coverage to 86%); the
-- cross-PR slot gate (scripts/ci/check_migration_slots.sh) flags
-- the conflict and PR 671 renumbers to 150 to avoid the goose
-- "duplicate version 149" panic. The fence holds slot 149 in
-- PR 671's branch so the embedded migration set stays contiguous
-- 1..N per ADR-041 / migrations/embed_test.go::TestMigrationsContiguous.
--
-- The fence will be deleted when one of the two PRs lands:
--   - If PR #540 lands first, slot 149 has its real migration;
--     PR #671 deletes this fence on rebase.
--   - If PR #671 lands first, slot 149 holds this no-op fence;
--     PR #540 renumbers to 151+ and deletes the fence on rebase.
--
-- Either way the chain stays contiguous. The fence is a no-op
-- `SELECT 1;` per ADR-041's reservation pattern (mirrors
-- 00146_reserve_slot.sql). The unique-prefix test
-- (TestMigrationsUniquePrefixes) exempts reservation filenames
-- so the carve-out is sound: this fence does not register as
-- slot 149 in the unique-prefix check; only the real schema
-- in PR #540 does.

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
