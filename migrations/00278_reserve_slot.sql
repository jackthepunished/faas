-- +goose Up
-- +goose StatementBegin
--
-- 00278_reserve_slot.sql — slot reservation placeholder
-- (ADR-041 / cross-PR gate carve-out).
--
-- This file is a deliberate no-op kept only to satisfy the
-- migrations/embed_test.go::TestMigrationsContiguous requirement
-- that the embedded migration set is exactly {1, 2, …, N} with
-- no gaps. It carries no schema change and does not appear in any
-- apply path beyond goose writing a row to goose_db_version.
--
-- Context: slot 00278 is claimed by open PR #910
-- (triggers_payload_max). PR #829 is junior on 00278. The branch
-- tip carries this 00278 fence so TestMigrationsContiguous stays
-- green until PR #910 lands. When #910 lands, this fence becomes
-- a duplicate-version collision and must be removed in a follow-up
-- commit on PR #829.
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
