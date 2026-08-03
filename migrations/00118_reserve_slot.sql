-- +goose Up
-- +goose StatementBegin
--
-- 00118_reserve_slot.sql — slot reservation placeholder
-- (ADR-041 / PR #391 migration gate carve-out).
--
-- Held by the Tier A7 edge-split PR cluster (ADR-070) so the
-- embedded migration set stays contiguous 1..118 while
-- follow-on PRs (cert-bundle audit, hint retention, LB-coordination
-- tokens per ADR-069) are still open. Drop this file on rebase if
-- the follow-on PR claims slot 118 first.
--
-- Body: `select 1;` — executes against the live DB at apply time but
-- produces no schema change. Same shape as migrations/00112_reserve_slot.sql
-- and migrations/00113_reserve_slot.sql.
--
select 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- No-op: nothing to reverse (the Up body is a deliberate select 1;).
-- +goose StatementEnd