-- filename: 00140_reserve_slot.sql
-- Slot fence: PR #654 merged (per-deployment require_authn at
-- slot 143); this fence at 140 now only bridges PR #651 (issue
-- #464 mega-PR) which still claims 140. PR #651 will resolve
-- this fence on its branch's merge via
-- `git rm migrations/00140_reserve_slot.sql`. The IAM mega-PR
-- (PR #653) originally held the sessions binding-hash migration
-- at slot 140, then renumbered to 142 after the cross-PR slot
-- gate surfaced PR #651's open claim. ADR-041.

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
