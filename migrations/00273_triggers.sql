-- filename: 00273_triggers.sql
-- Trigger primitive (event-source mappings, closes #757).
--
-- Adds the unified `triggers` table — one resource with a `kind`
-- discriminator covering cron, kafka, nats, redis_streams, sqs_compat,
-- and in-platform queue/delayed_task (per ADR-0NN). Backed by
-- trigger_records (one row per source record, ReportBatchItemFailures
-- atomic unit) and trigger_dead_letter (closed-vocab failure routing).
--
-- Default-OFF in this PR: no production code path writes or reads these
-- tables until layer 3 (dispatch_triggers.go) lands in a follow-up commit.
-- The schema is gated by the TriggersAllowed plan cap (Free=0) so even
-- if a customer finds the API surface, Hobby+ opt-in is required.
--
-- Cross-references: ADR-099 jobs cluster precedent (single migration
-- per concern, default-OFF staging), ADR-098 single-flight wake gate,
-- ADR-090 cron-fire-now manifest slot (kind='cron' is a thin pointer
-- to the existing `crons` table).

-- +goose Up
-- +goose StatementBegin

-- A) The unified Trigger resource.
CREATE TABLE triggers (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id      UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    app_id          UUID NOT NULL REFERENCES apps(id)    ON DELETE CASCADE,
    kind            TEXT NOT NULL
        CHECK (kind IN ('cron','kafka','nats','redis_streams','sqs_compat','queue')),
    slug            TEXT NOT NULL,
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    config          JSONB NOT NULL DEFAULT '{}'::jsonb,
    batch_size_max  INT NOT NULL DEFAULT 64
        CHECK (batch_size_max BETWEEN 1 AND 5000),
    batch_window_ms INT NOT NULL DEFAULT 1000
        CHECK (batch_window_ms BETWEEN 10 AND 600000),
    max_attempts    INT NOT NULL DEFAULT 5
        CHECK (max_attempts BETWEEN 1 AND 25),
    cron_id         UUID NULL UNIQUE REFERENCES crons(id) ON DELETE CASCADE,
    source          TEXT NULL
        CHECK (source IS NULL OR source IN ('queue','delayed_task')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (
        (kind = 'cron'  AND cron_id IS NOT NULL AND source IS NULL) OR
        (kind <> 'cron' AND cron_id IS NULL)
    ),
    UNIQUE (app_id, slug)
);
CREATE INDEX triggers_account_kind_idx ON triggers(account_id, kind);
CREATE INDEX triggers_app_kind_enabled ON triggers(app_id, kind) WHERE enabled;
CREATE INDEX triggers_cron_id_idx      ON triggers(cron_id) WHERE cron_id IS NOT NULL;

-- B) One row per source record (Lambda ESM shape, ReportBatchItemFailures).
CREATE TABLE trigger_records (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    trigger_id      UUID NOT NULL REFERENCES triggers(id) ON DELETE CASCADE,
    item_identifier TEXT NOT NULL,
    payload         JSONB NOT NULL DEFAULT '{}'::jsonb,
    headers         JSONB NOT NULL DEFAULT '{}'::jsonb,
    metadata        JSONB NOT NULL DEFAULT '{}'::jsonb,
    state           TEXT NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending','claimed','succeeded','retry','dead_letter')),
    attempts        INT  NOT NULL DEFAULT 0
        CHECK (attempts >= 0 AND attempts <= 25),
    next_fire_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    received_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_error      TEXT NULL,
    last_dispatched_at TIMESTAMPTZ NULL,
    UNIQUE (trigger_id, item_identifier)
);
CREATE INDEX trigger_records_due_idx
    ON trigger_records(trigger_id, next_fire_at)
    WHERE state IN ('pending','retry');
CREATE INDEX trigger_records_dlq_idx
    ON trigger_records(trigger_id, state)
    WHERE state = 'dead_letter';

-- C) Closed-vocab failure routing for terminal records.
CREATE TABLE trigger_dead_letter (
    record_id   UUID PRIMARY KEY REFERENCES trigger_records(id) ON DELETE CASCADE,
    trigger_id  UUID NOT NULL,
    reason      TEXT NOT NULL
        CHECK (reason IN ('rate_limited','poison_record','max_attempts','broker_error',
                          'plan_quota','payload_too_large','customer_disabled')),
    routed_to   TEXT NOT NULL
        CHECK (routed_to IN ('drop','manual_retry','customer_dlq')),
    detail      JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX trigger_dlq_trigger_reason_idx ON trigger_dead_letter(trigger_id, reason);

-- D) Widen invocations.source enum to admit 'esm' for batch dispatch.
-- Lambda ESM dispatches share the same wake path as customer HTTP traffic
-- (per pkg/sched/drain.go:65-67 — "no second admission policy"). The
-- runner envelope is delivered to the same VM (instances.kind='wake',
-- not a new flavor).
ALTER TABLE invocations DROP CONSTRAINT invocations_source_check;
ALTER TABLE invocations ADD CONSTRAINT invocations_source_check
    CHECK (source IN ('async_invoke','queue','delayed_task','cron','replay','esm'));

-- E) pg_notify trigger (analog to invocation_due).
CREATE OR REPLACE FUNCTION trg_notify_trigger_ready() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('trigger_ready',
        json_build_object('trigger_id', NEW.trigger_id,
                          'record_id',  NEW.id,
                          'item_id',    NEW.item_identifier)::text);
    RETURN NEW;
END $$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_ready_notify
    AFTER INSERT ON trigger_records
    FOR EACH ROW EXECUTE FUNCTION trg_notify_trigger_ready();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS trigger_ready_notify ON trigger_records;
DROP FUNCTION IF EXISTS trg_notify_trigger_ready();

ALTER TABLE invocations DROP CONSTRAINT invocations_source_check;
ALTER TABLE invocations ADD CONSTRAINT invocations_source_check
    CHECK (source IN ('async_invoke','queue','delayed_task','cron','replay'));

DROP INDEX IF EXISTS trigger_dlq_trigger_reason_idx;
DROP TABLE IF EXISTS trigger_dead_letter;
DROP INDEX IF EXISTS trigger_records_dlq_idx;
DROP INDEX IF EXISTS trigger_records_due_idx;
DROP TABLE IF EXISTS trigger_records;
DROP INDEX IF EXISTS triggers_cron_id_idx;
DROP INDEX IF EXISTS triggers_app_kind_enabled;
DROP INDEX IF EXISTS triggers_account_kind_idx;
DROP TABLE IF EXISTS triggers;
-- +goose StatementEnd
