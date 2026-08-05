-- filename: 00139_reserve_slot.sql
-- Slot fence: PR #658 (issue #476 webhook delivery) claims slots
-- 140 + 141 on its branch (webhook tables + delivery log); this
-- fence at 139 is the bridge between PR #647's apps_eviction_priority
-- at 138 and PR #658's app_webhooks at 140. PR #658 will resolve
-- this fence on its branch's merge via `git rm migrations/00139_reserve_slot.sql`.
-- The IAM mega-PR (PR #653) originally held the api_keys provenance
-- migration at slot 139, then renumbered twice — to 141 after
-- PR #651's open claim surfaced, then to 144 after PR #658's
-- open claim on 141 surfaced. ADR-041.

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
