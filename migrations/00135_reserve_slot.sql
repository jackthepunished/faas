-- filename: 00135_reserve_slot.sql
-- Fence at slot 135 — held by PR #654 so the migration set stays
-- contiguous 1..N where N = 139 (after PR #654's renumber
-- cascade 00135 → 00137 → 00143). PR #654 wanted slot 135
-- originally, but four sibling PRs (#540, #647, #651, #653) also
-- claimed 135 in the same week, so per ADR-041 + memory
-- cross-pr-slot-gate-races-with-active-pr the real schema
-- renumbered twice. Slot 135 becomes a fence to bridge 00134
-- (api_keys_org_bound) and the cascading renumber chain.
-- Whichever sibling PR's real schema lands at 135 first must
-- `git rm migrations/00135_reserve_slot.sql` on merge.
--
-- ADR-041 (migration slot reservation convention): the fence body
-- is a no-op `select 1;` inside goose's StatementBegin/End markers
-- so goose applies it cleanly and writes a row in goose_db_version.

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
