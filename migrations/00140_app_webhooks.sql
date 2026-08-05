-- filename: 00140_app_webhooks.sql
-- +goose Up
-- Issue #476 — outbound webhook delivery table + dead-letter queue +
-- retry policy (ADR-076).
--
-- app_webhooks: per-app subscription row owned by apid. One row per
-- (app_id, target_url) — a customer that wants the same event
-- delivered to two endpoints registers two rows. The unique index
-- is the load-bearing guard against a re-create race; the apid handler
-- returns 409 (not 422) when a duplicate slips in.
--
-- Storage shape:
--   - app_id is the FK root — DeleteApp cascades to subscriptions and
--     deliveries (mirrors alert_rules.app_id behaviour, migration
--     00062_alert_rules.sql). account_id is denormalised onto every
--     row to back the dispatcher's per-account fairness claim query
--     (ORDER BY account_id, next_attempt_at) without joining apps.
--   - target_url is validated through pkg/oci/egress.go at apid
--     write time AND dial time (SSRF defence against DNS-rebinding,
--     spec §11 / ADR-034) — mirrors alert_rules.webhook_url.
--   - secret_sealed bytea is the age/X25519-sealed secret
--     (pkg/secretbox). The plain value never lands in SQL; the apid
--     handler seals on write and unseals on dispatch; reads return
--     a masked constant. Closes G2 (gap, §17) — env secrets are
--     sealed at rest, same as alert_rules.webhook_secret_sealed.
--   - event_filter is a jsonb array of event-name strings
--     (e.g. ['cron.fired','app.scaled']). An empty array means
--     "all events"; the evaluator on the dispatch side is a single
--     `WHERE $1 = ANY(event_filter)` lookup against the column. A
--     jsonb[] column (not jsonb) keeps the index lookup B-tree-clean
--     without a jsonb_path_ops GIN — see issue #476's "events
--     vocabulary" section in the dispatch commit.
--   - retry_policy is a closed vocabulary ('default' | 'aggressive'
--     | 'none'). The dispatcher reads this column when computing
--     backoff; new policies land as a controller addition first,
--     the CHECK is the floor a typo cannot cross.
--   - enabled toggles subscription-level dispatch without losing
--     the row; a customer can pause + resume without losing the
--     delivery ledger. The handler mirrors this at updateAppWebhook.
create table if not exists app_webhooks (
    id              uuid primary key default gen_random_uuid(),
    app_id          uuid not null references apps(id) on delete cascade,
    account_id      uuid not null references accounts(id) on delete cascade,
    target_url      text not null,
    secret_sealed   bytea not null,
    event_filter    jsonb not null default '[]'::jsonb,
    retry_policy    text not null default 'default',
    enabled         boolean not null default true,
    created_at      timestamptz not null default now(),
    updated_at      timestamptz not null default now(),
    constraint app_webhooks_retry_policy_chk check (
        retry_policy in ('default','aggressive','none')
    ),
    constraint app_webhooks_target_url_len_chk check (
        char_length(target_url) between 8 and 2048
    )
);

-- Index plan:
--   - app_webhooks_account_idx: dispatcher's per-account fairness
--     claim query joins on account_id and the (account_id, created_at
--     desc) GET-deliveries endpoint. Plain B-tree keeps writes cheap.
--   - app_webhooks_app_target_uniq: one row per (app, target_url);
--     the apid handler returns 409 on a duplicate INSERT.
create index if not exists app_webhooks_account_idx
    on app_webhooks (account_id);
create unique index if not exists app_webhooks_app_target_uniq
    on app_webhooks (app_id, target_url);

-- +goose StatementEnd

-- +goose Down
-- Forward-only: dropping app_webhooks would silently lose every
-- delivery ledger row via FK cascade and break the dispatcher's
-- claim query. An operator-driven downgrade preserves data
-- (mirrors migrations/00131_apps_align_min_instances.sql and
-- 00138_apps_eviction_priority.sql — forward-only is the
-- project-wide policy on subscriber tables that feed schedd).
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd