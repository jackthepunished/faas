-- +goose Up
-- +goose StatementBegin
--
-- 00081_reserve_slot.sql — slot reservation placeholder
-- (ADR-041 / PR #391 migration gate carve-out).
--
-- This file is a deliberate no-op kept only to satisfy the
-- migrations/embed_test.go::TestMigrationsContiguous requirement
-- that the embedded migration set is exactly {1, 2, …, N} with
-- no gaps.
--
select 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- No-op: nothing to reverse.
-- +goose StatementEnd
