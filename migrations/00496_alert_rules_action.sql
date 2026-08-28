-- filename: 00496_alert_rules_action.sql
-- +goose Up
-- +goose StatementBegin

-- Issue #976 / ADR-122 (SAFE-RELEASES-B) — alert-rule `action`
-- column that routes a fired alert into the new pkg/alerts.ActionExecutor
-- seam (commit 4). Backward-compatible: every existing alert_rules row
-- defaults to 'webhook' (the legacy Dispatcher fan-out) so the
-- evaluator's existing behaviour is preserved for every pre-PR rule.
--
-- Closed set:
--   webhook  — Dispatcher fan-out (existing behaviour, default)
--   rollback — ActionExecutor.Execute(client.RollbackTo) — restore the
--              prior live deployment for the rule's app
--   demote   — ActionExecutor.Execute(client.PatchDeploymentsIdTraffic, 0)
--              — pin the rule's current canary step at 0%
--   promote  — ActionExecutor.Execute(client.PatchDeploymentsIdTraffic, 100)
--              — short-circuit the canary ladder to 100%
--
-- Rules with action in {rollback,demote,promote} still ALSO fire the
-- Dispatcher webhook — the evaluator calls both consumers. This way
-- a customer can wire Slack notifications (webhook) AND auto-rollback
-- (action=rollback) on the same rule. The dispatchMu in evaluator.go
-- serialises both consumers to keep webhook + action ordering
-- deterministic.
--
-- Back-compat: NOT NULL DEFAULT 'webhook' is metadata-only on pre-PR
-- rows (PG11+ fast-default). Existing webhook-only rules need zero
-- migration logic.

ALTER TABLE alert_rules
    ADD COLUMN IF NOT EXISTS action text NOT NULL DEFAULT 'webhook';

ALTER TABLE alert_rules
    DROP CONSTRAINT IF EXISTS alert_rules_action_chk;
ALTER TABLE alert_rules
    ADD CONSTRAINT alert_rules_action_chk
        CHECK (action IN ('webhook','rollback','demote','promote'));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE alert_rules
    DROP CONSTRAINT IF EXISTS alert_rules_action_chk;
ALTER TABLE alert_rules
    DROP COLUMN IF EXISTS action;

-- +goose StatementEnd
