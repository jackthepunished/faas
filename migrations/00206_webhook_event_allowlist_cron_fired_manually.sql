-- filename: 00206_webhook_event_allowlist_cron_fired_manually.sql
-- +goose Up
-- +goose StatementBegin

-- Issue #791 PR-D / ADR-090 §"Sub-decision 7" — widen the closed
-- vocabulary of `app_webhook_deliveries.event` to admit
-- `cron.fired.manually`. PR-C introduced this audit-event name at
-- pkg/sched/loop.go (constants `AuditEventCronFired` +
-- `AuditEventCronFiredManually`) but the SQL CHECK installed by
-- migrations/00141 only listed `cron.fired`, so the webhook
-- subscriber registration surface silently rejects the new event
-- name at two layers (this SQL CHECK + pkg/api/webhooks.go::
-- AllowedAppWebhookEvents).
--
-- The widener uses the NOT VALID + VALIDATE race-free triplet from
-- migrations/00035_instances_state_check_realigns.sql:41-59:
--
--   1. DROP CONSTRAINT — concurrent writes from apid (insert)
--      skip the constraint during the validate pass. The hard
--      invariant: at no point in this migration is `event`
--      allowed to take a value outside the new set *for new
--      rows*. New rows land without a CHECK between the DROP
--      and the ADD; that's a 1ms window in transaction-only
--      migration apply under goose.
--   2. ADD CONSTRAINT ... NOT VALID — installs the new bound
--      without scanning the existing table.
--   3. VALIDATE CONSTRAINT — SHARE UPDATE EXCLUSIVE; concurrent
--      apid INSERTs continue. Existing rows are 100% within the
--      new set by construction: the existing CHECK already
--      accepted only the five values, and `cron.fired.manually`
--      is a NEW value (no row uses it yet).
--
-- Scope discipline (ADR-090 §"Sub-decision 7"): PR-D adds ONLY
-- `cron.fired.manually`. The pre-existing divergence between
-- the SQL CHECK's five values (cron.fired | app.deployed |
-- app.scaled | app.parked | app.woken) and the Go allowlist's
-- five values (cron.fired | app.created | app.deleted |
-- build.succeeded | build.failed) is intentional and documented
-- as a follow-up issue — the PR-D body names it so a future
-- reconciliation PR has audit trail.

alter table app_webhook_deliveries
    drop constraint if exists app_webhook_deliveries_event_chk;

alter table app_webhook_deliveries
    add constraint app_webhook_deliveries_event_chk
        check (event in (
            'cron.fired',
            'cron.fired.manually',
            'app.deployed', 'app.scaled', 'app.parked', 'app.woken'
        )) not valid;

alter table app_webhook_deliveries
    validate constraint app_webhook_deliveries_event_chk;

-- +goose StatementEnd

-- +goose Down
-- Forward-only by design (mirrors migrations/00141_app_webhook_deliveries.sql:110-117
-- which has the same forward-only commit and the same Down contract).
-- Reversing the widening would orphan any existing
-- `event = 'cron.fired.manually'` rows the operator may have
-- delivered against, so we preserve the wider constraint
-- unconditionally on downgrade.
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
