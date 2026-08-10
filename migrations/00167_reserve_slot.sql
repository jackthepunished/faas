-- +goose Up
-- +goose StatementBegin
--
-- 00167_reserve_slot.sql — slot reservation placeholder
-- (ADR-041 / PR #391 migration gate carve-out).
--
-- This file is a deliberate no-op kept only to satisfy the
-- migrations/embed_test.go::TestMigrationsContiguous requirement
-- that the embedded migration set is exactly {1, 2, …, N} with
-- no gaps. It carries no schema change and does not appear in any
-- apply path (the replay-safety gate in ci.yml drops files whose
-- basename matches the reservation regex from its "added
-- migration versions" computation).
--
-- Slot 167 is held for PR #798 (apps_overflow_node, Tier A10).
-- Tier A9 / ADR-089 (PR #797) lands at 00168.
--
-- Body: `select 1;` — executes against the live DB at apply time
-- but produces no schema change.
--
select 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- No-op: nothing to reverse (the Up body is a deliberate select 1;).
-- +goose StatementEnd
