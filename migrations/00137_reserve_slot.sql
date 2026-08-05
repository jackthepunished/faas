-- filename: 00137_reserve_slot.sql
-- Slot fence: see 00135_reserve_slot.sql's header for the
-- cross-PR slot-cluster rationale (PR #540 webhook-deliveries,
-- PR #651 deploy-scans, PR #653 IAM provenance, PR #654
-- per-deployment authn). PR #654 holds slot 137 for
-- 00137_apps_require_authn.sql on its branch; PR #647 (issue
-- #475 / ADR-075) fences this slot on its branch to keep the
-- embedded set contiguous while eviction_priority lands at 138.
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
