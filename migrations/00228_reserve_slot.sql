-- filename: 00228_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
-- Reserve slot 228 as a cross-PR coexistence fence for PR #867
-- (maintenance PR-A, ADR-091 amendment) which claims 00228 with
-- 00228_apps_maintenance_mode.sql. PR #845 (ADR-091 D21 kind=geo)
-- renumbered AWAY from 00226 to 00229, passing through 00228 on
-- the way; this fence exists so the migrations-contiguous gate
-- passes locally without colliding with PR #867's real schema.
-- The fence is reservation-only per ADR-041 and
-- cross-pr-slot-gate-reservation-fence-pattern, so it does NOT
-- collide with PR #867's real migration — the slot-check excludes
-- reservation files from the overlap scan. Once #867 merges into
-- main, this fence is deleted via
-- `git rm migrations/00228_reserve_slot.sql` (per
-- cross-pr-rebase-fence-deletion-hazard).
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd