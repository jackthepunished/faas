-- filename: 00139_reserve_slot.sql
-- Slot fence: see 00135_reserve_slot.sql's header for the
-- cross-PR slot-cluster rationale (PR #540 webhook-deliveries,
-- PR #651 deploy-scans, PR #653 IAM provenance, PR #654
-- per-deployment authn). The IAM mega-PR (PR #653) originally
-- landed the api_keys provenance migration at slot 139, then
-- renumbered a second time to 141 after the cross-PR slot gate
-- surfaced that open PRs #651 and #654 both also claim 139.
-- PR #651 will resolve this fence on its branch's merge;
-- PR #654 will resolve the same fence from its side.
-- ADR-041.

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
