-- filename: 00139_reserve_slot.sql
-- Slot fence: PR #654 (per-deployment require_authn) merged and
-- landed its real schema at slot 143; this fence at 139 now only
-- bridges PR #651 (issue #464 mega-PR) which still claims 139.
-- PR #651 will resolve this fence on its branch's merge via
-- `git rm migrations/00139_reserve_slot.sql`. The IAM mega-PR
-- (PR #653) originally held the api_keys provenance migration at
-- slot 139, then renumbered to 141 after the cross-PR slot gate
-- surfaced PR #651's open claim. ADR-041.

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
