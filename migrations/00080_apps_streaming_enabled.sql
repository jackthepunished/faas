-- filename: 00080_apps_streaming_enabled.sql
-- +goose Up
-- Add per-app streaming flag (issue #471). Streams the response body
-- from the guest through gatewayd-internal to the client with periodic flushes
-- (200 ms / 256 KiB) so ADR-046 tx_bytes stays accurate and LLM-style
-- token streams / large JSON exports / SSE work.
--
-- One column:
--   streaming_enabled  boolean NOT NULL DEFAULT false
--
-- Default false keeps every pre-existing app on the buffered path —
-- opt-in by the customer via PATCH /v1/apps/{slug}. apid's plan gate
-- (CodePlanStreamingNotAllowed) refuses Free customers from setting
-- it to true; the default-by-plan is applied at create time in
-- cmd/apid/handlers.go::buildApp using pkg/api/limits.go.
--
-- Replay-safe (PR #377 / ADR-041): ADD COLUMN IF NOT EXISTS. The column
-- is a single NOT NULL boolean with a constant default — no rewrite,
-- no index bloat, and a second MigrateUp is a no-op.
--
-- Partial index on streaming_enabled=true is small (Hobby+/Pro/Scale
-- apps that opted in); gatewayd-internal's lookup is a per-request cache hit
-- on apps[host] so this index is for the operator "which apps stream?"
-- query path (dashboard), not the hot request path.
-- +goose StatementBegin
ALTER TABLE apps
    ADD COLUMN IF NOT EXISTS streaming_enabled boolean NOT NULL DEFAULT false;
CREATE INDEX IF NOT EXISTS apps_streaming_enabled_idx
    ON apps(streaming_enabled)
    WHERE streaming_enabled = true;
-- +goose StatementEnd

-- +goose Down
-- Reverse: drop the partial index, then drop the column. A row that
-- had streaming_enabled=true loses the bit on downgrade; the GET
-- /v1/apps/{slug} response shape omits streaming_enabled because the
-- column no longer exists, which is the correct degraded behaviour.
-- +goose StatementBegin
DROP INDEX IF EXISTS apps_streaming_enabled_idx;
ALTER TABLE apps
    DROP COLUMN IF EXISTS streaming_enabled;
-- +goose StatementEnd