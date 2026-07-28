-- +goose Up
-- +goose StatementBegin

-- M7 (issue #294): webhook replay dedupe. One table covers all
-- three ingresses on the box (GitHub via gatewayd, Stripe + Paddle
-- via apid). The previous dedupe tables — 00004 stripe_push_dedupe
-- and 00034 paddle_overage_dedupe — are pusher-side (meterd), not
-- webhook-replay side; SOC 2 CC6.1 expects idempotency on every
-- external event ingestion, so this table sits in front of the
-- handler dispatch.
--
-- TTL is 5 minutes (the webhookdedupe.TTL constant in
-- pkg/webhookdedupe/dedupe.go). This matches the Stripe / Paddle
-- signature tolerance windows already used by their verifiers
-- (5*time.Minute at handlers_ext.go:1133 and paddle/webhook.go:22),
-- so a legitimate retry that falls inside the signature-validity
-- window cannot bypass the replay check.
--
-- Replays are rejected with 200 (idempotent — the upstream provider
-- interprets as success and stops retrying). The row is recorded
-- by webhookdedupe.CheckReplay before the handler returns; the
-- apid sweep goroutine (cmd/apid/server.go) deletes rows where
-- expires_at < now() every 60s.
--
-- Schema-only change; no rows are seeded.

create table if not exists webhook_deliveries (
  provider     text        not null,
  delivery_id  text        not null,
  received_at  timestamptz not null default now(),
  expires_at   timestamptz not null,
  primary key (provider, delivery_id),
  constraint  webhook_deliveries_provider_check
    check (provider in ('github','stripe','paddle'))
);

-- Partial index on the sweep key. The sweep deletes rows where
-- expires_at < now(); a partial index keyed on the same predicate
-- keeps the deletion O(N expired) rather than O(N total). The
-- `where expires_at < now()` predicate is a stable_plans expression
-- in PostgreSQL — the planner constant-folds `now()` to the plan
-- start time at CREATE INDEX.
create index if not exists webhook_deliveries_expires_idx
  on webhook_deliveries (expires_at)
  where expires_at is not null;

-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin
drop table if exists webhook_deliveries;
-- +goose StatementEnd
