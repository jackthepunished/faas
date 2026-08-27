-- filename: 00472_events_operator_intents_trace_id.sql
-- +goose Up
-- +goose StatementBegin
--
-- Trace IDs for the operator-action observability layer (PR #TBD).
--
-- Closes the operator-correlatability gap that PR #1099 left: the
-- enqueue audit row (apid → events) and the terminal outcome
-- audit row (schedd → events) carry no shared identifier tying
-- them to the inbound HTTP request that caused them. With this
-- migration every operator-action row stamped into `events` and
-- every `operator_intents` row carries a 32-char OTel W3C trace-id
-- so the dashboard can join alert ↔ action ↔ outcome on one
-- column.
--
-- Why a dedicated column and not the existing `data` jsonb:
--   * the trace_id_completeness gauge, the partial-index join,
--     and the future dashboard tile all need a column.
--   * jsonb-key lookups are seqscan hazards at the row counts
--     `events` already has.
--
-- Format choice: OTel W3C 32-char hex (text column with regex
-- CHECK). Matches the precedent at 00427_request_telemetry.sql
-- (`trace_id text CHECK (trace_id IS NULL OR trace_id ~ '^[0-9a-f]{32}$')`).
-- Same format on both tables lets a single index strategy serve
-- both, and lets the existing `pkg/audit/audit.go:221-232`
-- OTel-derived `data["trace_id"]` lift flow into the column
-- unchanged when a span context is present.
--
-- Nullable, no default: pre-PR rows + cron-fired rows without an
-- inbound trace_id keep their existing NULL shape. The
-- `events.trace_id IS NOT NULL` predicate bounds the partial
-- index to operator-action rows (where trace_id is expected to
-- be set), keeping index size proportionate to the writer
-- surface that stamps a trace_id.
--
-- Mirrors the existing 00427 + 00445 partial-index shape so the
-- planner treats `events.trace_id` and
-- `operator_intents.trace_id` as the same family.

ALTER TABLE events
    ADD COLUMN IF NOT EXISTS trace_id text
        CHECK (trace_id IS NULL OR trace_id ~ '^[0-9a-f]{32}$');

CREATE INDEX IF NOT EXISTS events_trace_idx
    ON events (trace_id)
    WHERE trace_id IS NOT NULL;

ALTER TABLE operator_intents
    ADD COLUMN IF NOT EXISTS trace_id text
        CHECK (trace_id IS NULL OR trace_id ~ '^[0-9a-f]{32}$');

CREATE INDEX IF NOT EXISTS operator_intents_trace_idx
    ON operator_intents (trace_id)
    WHERE trace_id IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS operator_intents_trace_idx;
ALTER TABLE operator_intents DROP COLUMN IF EXISTS trace_id;
DROP INDEX IF EXISTS events_trace_idx;
ALTER TABLE events DROP COLUMN IF EXISTS trace_id;
-- +goose StatementEnd