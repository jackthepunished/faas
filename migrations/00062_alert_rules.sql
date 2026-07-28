-- +goose Up
-- +goose StatementBegin

-- Issue #396 — customer-configurable alert rules (ADR-045).
-- Account-scoped webhook delivery for the {error rate, latency p50/p95/p99,
-- cold-start %, request count, scheduled-work failure count} condition
-- model, evaluated by meterd (issue #396 PR 4). One row per rule; an
-- account-wide rule has app_id NULL.
--
-- Storage shape:
--   - account_id is the FK root — DeleteAccount cascades to the rules and
--     deliveries (migrations/00031_account_grace.sql, PR #74).
--   - app_id is NULLABLE on purpose: criterion 2 says "one app OR all apps".
--     A NULL app_id is an account-wide rule. The handler resolves NULL to
--     "every non-deleted app on the account" at evaluation time.
--   - metric is a closed vocabulary from pkg/api.AppMetricsResponse. New
--     metrics land first as a controller addition; the table CHECK is the
--     floor a typo cannot cross (mirrors apps.slug and api_keys.scopes).
--   - comparison is the textual enum ('gt','gte','lt','lte'). Symbolic
--     operators (`>`, `>=`) would force escaping in JSON + the OpenAPI
--     schema and invite CHECK-vs-payload drift.
--   - window_spec is the same closed vocabulary /v1/apps/{slug}/metrics
--     accepts (5m..15d, migration 00023 metrics range vocabulary). The
--     evaluator cannot ask Prometheus for data outside prom_retention_days
--     because the vocabulary itself is bounded.
--   - failure_source is NULLABLE; the XOR CHECK below ties it to
--     metric='failed_invocations'. All other metrics ignore it.
--   - webhook_url stores the customer-supplied destination; reused through
--     pkg/oci/egress.go at validation AND dial time (SSRF defence against
--     DNS-rebinding, spec §11 / ADR-034).
--   - webhook_secret_sealed is the age/X25519-sealed secret (pkg/secretbox).
--     The plain value never lands in SQL — the apid handler seals on write
--     and unseals on dispatch; reads return a masked constant. Closes
--     G2 (gap, §17) — env secrets are sealed at rest.
--   - state is the cool-down primitive: 'ok'→'firing' on a real breach,
--     'firing'→'ok' on a healthy evaluation. While 'firing', subsequent
--     breaching ticks do nothing. The atomic ClaimAlertFire CTE in
--     pgstore.go re-implements this on the dedupe side for redelivery
--     races; state and the UNIQUE idempotency_key UN-below cover
--     different failure modes.
--   - cooldown_minutes bounds the per-rule minimum, mirroring the
--     reuse-temperature on the WebhookReplayDedup table.

create table if not exists alert_rules (
    id                   uuid primary key default gen_random_uuid(),
    account_id           uuid not null references accounts(id) on delete cascade,
    app_id               uuid references apps(id) on delete cascade,
    name                 text not null,
    enabled              boolean not null default true,
    metric               text not null,
    comparison           text not null,
    threshold            double precision not null,
    window_spec          text not null,
    failure_source       text,
    webhook_url          text not null,
    webhook_secret_sealed bytea not null,
    cooldown_minutes     int not null default 30,
    state                text not null default 'ok',
    last_fired_at        timestamptz,
    last_evaluated_at    timestamptz,
    created_at           timestamptz not null default now(),
    updated_at           timestamptz not null default now(),
    constraint alert_rules_metric_chk check (metric in (
        'error_rate_pct',
        'latency_p50_ms', 'latency_p95_ms', 'latency_p99_ms',
        'cold_start_pct', 'request_count',
        'failed_invocations'
    )),
    constraint alert_rules_comparison_chk check (comparison in ('gt','gte','lt','lte')),
    constraint alert_rules_window_chk check (window_spec in
        ('5m','15m','1h','6h','24h','7d','15d')),
    constraint alert_rules_failure_source_chk check (
        failure_source is null or failure_source in
        ('any','cron','queue','delayed_task','async_invoke')
    ),
    constraint alert_rules_failure_source_xor_chk check (
        (metric = 'failed_invocations' and failure_source is not null) or
        (metric <> 'failed_invocations' and failure_source is null)
    ),
    constraint alert_rules_state_chk check (state in ('ok','firing')),
    constraint alert_rules_cooldown_chk check (
        cooldown_minutes between 5 and 1440
    ),
    constraint alert_rules_name_len_chk check (
        char_length(name) between 1 and 64
    )
);

-- Index plan:
--   - enabled scan: evaluator's hot loop (ListEnabledAlertRules).
--     Partial index keeps disabled rows out of the read path.
--   - unique (account_id, name): a rename path would split this into a
--     partial unique on (account_id, name) WHERE deleted_at IS NULL; today
--     deletes are hard and a flat unique is correct.
create index if not exists alert_rules_enabled_idx
    on alert_rules (account_id) where enabled = true;
create unique index if not exists alert_rules_account_name_uniq
    on alert_rules (account_id, name);

-- alert_deliveries: one row per fire. idempotency_key UNIQUE is the
-- dedupe primitive; rejects a second dispatch inside the same
-- cool-down bucket (rule_id + floor(epoch/cooldown_seconds)) even
-- across a meterd restart. Every attempt mutates the row in place
-- (attempt_count, last_status_code, last_error, status,
-- delivered_at) — sibling attempts table is over-engineering for
-- the 5-attempt cap.
create table if not exists alert_deliveries (
    id               uuid primary key default gen_random_uuid(),
    rule_id          uuid not null references alert_rules(id) on delete cascade,
    account_id       uuid not null references accounts(id) on delete cascade,
    app_id           uuid,
    idempotency_key  text not null,
    payload          jsonb not null,
    status           text not null default 'pending',
    attempt_count    int  not null default 0,
    last_status_code int,
    last_error       text,
    observed_value   double precision not null,
    fired_at         timestamptz not null default now(),
    delivered_at     timestamptz,
    constraint alert_deliveries_status_chk check (
        status in ('pending','delivered','failed')
    )
);

create unique index if not exists alert_deliveries_idempotency_uniq
    on alert_deliveries (idempotency_key);

-- ListAlertDeliveriesForRule's ORDER BY fired_at DESC — the dashboard's
-- "recent deliveries" pane.
create index if not exists alert_deliveries_rule_fired_idx
    on alert_deliveries (rule_id, fired_at desc);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

drop index if exists alert_deliveries_rule_fired_idx;
drop index if exists alert_deliveries_idempotency_uniq;
drop table if exists alert_deliveries;

drop index if exists alert_rules_account_name_uniq;
drop index if exists alert_rules_enabled_idx;
drop table if exists alert_rules;

-- +goose StatementEnd
