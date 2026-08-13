-- filename: 00226_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
-- Reserve slot 226 as a cross-PR coexistence fence for PR #864
-- (reqbudget PR1, ADR-093) which claims 00226 with
-- 00226_edge_rules_kind_budget.sql. PR #845 (ADR-091 D21 kind=geo)
-- renumbered AWAY from 00226 to 00229 to avoid stepping on PR
-- #864's claim. This fence is reservation-only per ADR-041 and
-- cross-pr-slot-gate-reservation-fence-pattern, so it does NOT
-- collide with PR #864's real schema — the slot-check excludes
-- reservation files from the overlap scan. The fence exists on
-- THIS branch so the migrations-contiguous gate passes locally;
-- PR #864's real migration lives on its branch. Once #864
-- merges into main, this fence is deleted via
-- `git rm migrations/00226_reserve_slot.sql` (per
-- cross-pr-rebase-fence-deletion-hazard).
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd