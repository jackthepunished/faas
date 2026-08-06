-- +goose Up
-- +goose StatementBegin

-- 00149_webhook_deliveries.sql — the missing backing table for the
-- Store webhook-replay-dedupe surface (issue #294).
--
-- PgStore.CheckWebhookReplay / RecordWebhookDelivery /
-- SweepExpiredWebhookDeliveries have referenced a `webhook_deliveries`
-- table since PR #294 landed, but no migration ever created it — the
-- comment in pgstore.go attributed it to "migration 00059", which is
-- actually the github_installations table (PR-C). Every call against
-- the PgStore path therefore raised SQLSTATE 42P01 (relation does not
-- exist). Production webhook ingresses masked the gap because
-- pkg/webhookdedupe's in-memory sync.Map adapter is what the handlers
-- actually use; the store methods are the durability-net that was
-- documented but never shipped.
--
-- This migration creates exactly the shape the three methods query:
--
--   * (provider, delivery_id) PK — the dedupe key. The unique
--     constraint is what makes a re-POSTed webhook within the TTL
--     window a 200-on-replay no-op.
--   * received_at — stamped at insert; CheckWebhookReplay tests
--     `received_at >= cutoff`.
--   * expires_at — the TTL anchor; SweepExpiredWebhookDeliveries
--     deletes rows with `expires_at < now()`. RecordWebhookDelivery
--     refreshes it via ON CONFLICT DO UPDATE.
--
-- The provider CHECK mirrors pkg/webhookdedupe's closed Provider set
-- (github | stripe | paddle) so a typo cannot silently create a
-- dedupe namespace that never collides with the ingress lookups.

create table if not exists webhook_deliveries (
    provider    text        not null check (provider in ('github', 'stripe', 'paddle')),
    delivery_id text        not null,
    received_at timestamptz not null default now(),
    expires_at  timestamptz not null,
    primary key (provider, delivery_id)
);

-- The sweep reads `where expires_at < $1`; without this the predicate
-- is a seq scan over the whole dedupe table on every apid sweep tick.
create index if not exists webhook_deliveries_expires_idx on webhook_deliveries (expires_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

drop table if exists webhook_deliveries;

-- +goose StatementEnd
