-- filename: 00136_reserve_slot.sql
-- Slot fence: see 00135_reserve_slot.sql's header for the
-- cross-PR slot-cluster rationale (PR #540 webhook-deliveries,
-- PR #651 deploy-scans, PR #653 IAM provenance, PR #654
-- per-deployment authn). PR #647 (issue #475 / ADR-075) holds
-- this slot open while eviction_priority lands at 138. ADR-041.

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
