-- filename: 00141_app_webhook_deliveries.sql
-- +goose Up
-- Issue #476 — outbound webhook delivery ledger + dead-letter queue
-- (ADR-076). One row per (event, target webhook) emission. The
-- scheduler (cmd/schedd, pkg/webhook/dispatcher) claims due rows in
-- per-account round-robin batches, POSTs through pkg/webhookout, and
-- mutates the row in place until either status='succeeded' (HTTP 2xx)
-- or status='dead' (attempt >= 7 OR a terminal 4xx other than 408/429).
--
-- Why one row per (event × target) and not one row per attempt:
--   - The customer-facing GET /deliveries endpoint streams one row
--     per logical event; per-attempt history lives in delivery_attempts
--     only if we ever need it (we don't today). Mirrors
--     alert_deliveries' "every attempt mutates the row in place"
--     rationale at migrations/00062_alert_rules.sql:100-106.
--   - The dispatcher's claim query is a single partial-index lookup
--     (`WHERE status IN ('pending','in_flight') AND next_attempt_at <= now()
--     ORDER BY account_id, next_attempt_at LIMIT $1`) — a sibling
--     attempts table would force a join on the hot path.
--
-- Storage shape:
--   - webhook_id FK cascades: delete a subscription, drop its ledger.
--     Same pattern as alert_deliveries.rule_id.
--   - app_id and account_id are denormalised onto the row. The
--     dispatcher's claim query orders by account_id for fairness and
--     needs no join to apps. The per-account GET-deliveries endpoint
--     filters on account_id for the same reason.
--   - event is the closed vocabulary: cron.fired today; app.scaled,
--     app.deployed, app.parked, app.woken arrive with the lifecycle
--     event emissions (issue #294 follow-ups). New events land as
--     a controller + handler addition first; a CHECK keeps typos out.
--   - payload is the wire body the customer receives — a jsonb object
--     serialised verbatim. The dispatcher signs with HMAC-SHA256 over
--     `<unix>.<delivery_id>.<body>` using the unsealed secret.
--   - status is the dispatcher state machine:
--       pending   — ready to claim; next_attempt_at <= now()
--       in_flight — claimed by a dispatcher goroutine; will resolve
--                   to succeeded / failed / dead within the http
--                   timeout. The claim query holds a row-level lock
--                   for the duration of the SELECT FOR UPDATE; the
--                   in_flight row is the post-commit visible state.
--       succeeded — HTTP 2xx response; no further attempts.
--       failed    — retryable error (5xx/408/429/network); next
--                   attempt scheduled at attempt+1.
--       dead      — terminal: attempt >= 7 (exhausted budget) OR a
--                   non-retryable 4xx. The customer can POST
--                   /deliveries/{id}/retry to clear status back to
--                   pending and bump next_attempt_at to now().
--   - last_response_code + last_error are nullable; populated only
--     after attempt 1. They drive the operator's "why is this dead"
--     diagnosis from the GET /deliveries response.
--   - next_attempt_at is the dispatcher's claim predicate. Default
--     `now()` makes a fresh row claimable on the next tick.
create table if not exists app_webhook_deliveries (
    id                 uuid primary key default gen_random_uuid(),
    webhook_id         uuid not null references app_webhooks(id) on delete cascade,
    app_id             uuid not null,
    account_id         uuid not null,
    event              text not null,
    payload            jsonb not null,
    attempt            int  not null default 0,
    status             text not null default 'pending',
    last_error         text,
    last_response_code int,
    next_attempt_at    timestamptz not null default now(),
    delivered_at       timestamptz,
    created_at         timestamptz not null default now(),
    updated_at         timestamptz not null default now(),
    constraint app_webhook_deliveries_status_chk check (
        status in ('pending','in_flight','succeeded','failed','dead')
    ),
    constraint app_webhook_deliveries_event_chk check (
        event in (
            'cron.fired',
            'app.deployed','app.scaled','app.parked','app.woken'
        )
    ),
    constraint app_webhook_deliveries_attempt_chk check (
        attempt between 0 and 7
    )
);

-- Index plan:
--   - app_webhook_deliveries_pending_idx: PARTIAL index on the
--     dispatcher's hot path. WHERE status IN ('pending','in_flight')
--     keeps claimed/succeeded/dead rows out of the claim scan, so
--     the table grows without bloating the hot read. A future
--     partition strategy could replace this; today the partial index
--     is sufficient at expected fleet sizes (≤10k deliveries/day
--     per spec §12). Mirrors alert_rules_enabled_idx's
--     "partial index keeps disabled rows out" rationale at
--     migrations/00062_alert_rules.sql:90-96.
--   - app_webhook_deliveries_account_created_idx: backs GET
--     /v1/apps/{slug}/webhooks/{id}/deliveries with no sort step.
--     The descending created_at matches the dashboard's "recent
--     deliveries" pane orientation (mirrors
--     alert_deliveries_rule_fired_idx).
create index if not exists app_webhook_deliveries_pending_idx
    on app_webhook_deliveries (status, next_attempt_at)
    where status in ('pending','in_flight');
create index if not exists app_webhook_deliveries_account_created_idx
    on app_webhook_deliveries (account_id, created_at desc);

-- +goose StatementEnd

-- +goose Down
-- Forward-only: dropping the ledger would silently lose every
-- in-flight retry state and break the dispatcher's claim query.
-- Operator-driven downgrade preserves data (mirrors
-- 00140_app_webhooks.sql and 00138_apps_eviction_priority.sql —
-- forward-only on subscriber / state-machine tables).
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd