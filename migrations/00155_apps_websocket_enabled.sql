-- filename: 00155_apps_websocket_enabled.sql
-- +goose Up
-- Add per-app websocket / Upgrade-traffic flag (issue #676 /
-- ADR-080). When true, the gatewayd-internal Upgrade detector
-- routes inbound Connection: Upgrade + Upgrade: <token> requests
-- to the new rawStreamReverseProxy (which opens the
-- ForwardRawStream RPC and pumps raw bytes into the guest's
-- netns TCP socket). When false, an upgrade request gets 501
-- with x-faas-error-reason: websocket_not_on_plan.
--
-- One column:
--   websocket_enabled  boolean NOT NULL DEFAULT false
--
-- Default false keeps every pre-existing app on the buffered /
-- plain-HTTP path — opt-in by the customer via PATCH
-- /v1/apps/{slug}. apid's plan gate (CodePlanWebSocketNotAllowed,
-- mirroring CodePlanStreamingNotAllowed from issue #471) refuses
-- Free customers from setting it to true; the default-by-plan is
-- applied at create time in cmd/apid/handlers.go::buildApp using
-- pkg/api/limits.go::Plan.WebSocketEnabled (Free=false,
-- Hobby/Pro/Scale=true).
--
-- The 100 MiB per-request cap (pkg/api.RawStreamMaxRequestBytes,
-- exported from pkg/vmmdgrpc PR-1 review-fix #2) is enforced on
-- the vmmd side (init-frame clamp); gatewayd-internal's forwarder just
-- stamps the constant on the init frame. Plan-level per-session
-- byte cap (50 GiB/month) is a metering follow-up — see
-- docs/adr/080.
--
-- Replay-safe (PR #377 / ADR-041): ADD COLUMN IF NOT EXISTS. The
-- column is a single NOT NULL boolean with a constant default —
-- no rewrite, no index bloat, and a second MigrateUp is a no-op.
--
-- Partial index on websocket_enabled=true is small (Hobby+/Pro/
-- Scale apps that opted in); gatewayd-internal's lookup is a per-request
-- cache hit on apps[host] so this index is for the operator
-- "which apps open raw streams?" query path (dashboard), not the
-- hot request path. Mirrors 00080_apps_streaming_enabled.sql.
-- +goose StatementBegin
ALTER TABLE apps
    ADD COLUMN IF NOT EXISTS websocket_enabled boolean NOT NULL DEFAULT false;
CREATE INDEX IF NOT EXISTS apps_websocket_enabled_idx
    ON apps(websocket_enabled)
    WHERE websocket_enabled = true;
-- +goose StatementEnd

-- +goose Down
-- Reverse: drop the partial index, then drop the column. A row
-- that had websocket_enabled=true loses the bit on downgrade; the
-- GET /v1/apps/{slug} response shape omits websocket_enabled
-- because the column no longer exists, which is the correct
-- degraded behaviour (every app falls back to plain HTTP — no
-- Upgrade handshake reaches the guest).
-- +goose StatementBegin
DROP INDEX IF EXISTS apps_websocket_enabled_idx;
ALTER TABLE apps
    DROP COLUMN IF EXISTS websocket_enabled;
-- +goose StatementEnd