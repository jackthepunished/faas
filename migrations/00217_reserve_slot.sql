-- filename: 00217_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
-- Reserve slot 217 for PR #849 (ADR-092 PR-A app_secrets.scope,
-- migrations/00217_app_secrets_scope.sql). This branch (PR #845,
-- ADR-091 D21 kind=geo) is at slot 220; the fence here preserves
-- contiguity (00215 → 00216 → 00217 → 00218 → 00219 → 00220)
-- so the TestMigrationsContiguous gate in migrations/embed_test.go
-- does not trip on PR #845's rebase.
--
-- Cross-PR coordination reminder for whoever lands PR #849: when
-- you replace this fence with the real 00217_app_secrets_scope.sql,
-- DO NOT bump the slot — keep it at 00217. PR #845 is locked
-- to 00220 (the fenced 00218 + 00219 are gates for PR #845's
-- sibling PR-A PR-B, and must stay at those slots).
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
