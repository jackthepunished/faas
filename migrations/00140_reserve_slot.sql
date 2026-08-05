-- filename: 00140_reserve_slot.sql
-- Slot fence: see 00139_reserve_slot.sql's header for the
-- cross-PR slot-cluster rationale (PR #651 deploy-scans, PR #653
-- IAM hardening, PR #654 per-deployment authn). The IAM mega-PR
-- (PR #653) originally landed the sessions binding-hash
-- migration at slot 140, then renumbered a second time to 142
-- after the cross-PR slot gate surfaced slot-139 collisions
-- with open PRs #651 and #654. PR #651 will resolve this fence
-- on its branch's merge; PR #654 will resolve the same fence
-- from its side. ADR-041.

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
