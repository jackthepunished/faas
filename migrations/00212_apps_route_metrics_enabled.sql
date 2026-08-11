-- filename: 00212_apps_route_metrics_enabled.sql
-- +goose Up
-- Add per-app route-metrics opt-in flag (ADR-093). When true, the
-- gatewayd-internal handler emits three additional Prometheus
-- series keyed by an enumerated `route` label (method + raw path,
-- bounded per-app at 50 distinct entries with __route_other__ as
-- the non-evicting overflow bucket):
--
--   gateway_requests_total{app,plan,route,code}
--   gateway_request_duration_seconds{app,route,class}
--   gateway_request_failures_total{app,plan,route,code}
--
-- The in-memory reader at GET /v1/internal/apps/{slug}/routes on
-- the gatewayd-internal loopback control listener (reverse-proxied
-- by apid as GET /v1/apps/{slug}/routes) exposes the same map for
-- the per-app dashboard panel; the two surfaces share the
-- underlying routeLabelSet, so they cannot drift.
--
-- One column:
--   route_metrics_enabled  boolean NOT NULL DEFAULT false
--
-- Default false keeps every pre-existing app on the per-app
-- {app, class} histogram + {app, plan, code} counter (ADR-042 §2
-- cardinality math, preserved verbatim) — opt-in by the customer
-- via PATCH /v1/apps/{slug}. apid's plan gate
-- (CodePlanRouteMetricsNotAllowed, mirroring CodePlanWebSocketNotAllowed
-- from issue #676 / migration 00155) refuses Free customers from
-- setting it to true; the default-by-plan is applied at create time
-- in cmd/apid/handlers.go::buildApp using the new
-- Plan.RouteMetricsAllowed (Free=false, Hobby/Pro/Scale=true).
--
-- The operator-level kill-switch ([route_metrics] enabled in
-- cmd/gatewayd-internal/config.go) is AND-gated with this flag —
-- a misconfigured fleet (operator off, customer on) emits nothing.
--
-- Replay-safe (PR #377 / ADR-041): ADD COLUMN IF NOT EXISTS. The
-- column is a single NOT NULL boolean with a constant default —
-- no rewrite, no index bloat, and a second MigrateUp is a no-op.
--
-- Partial index on route_metrics_enabled=true is small (Hobby+/
-- Pro/Scale apps that opted in); gatewayd-internal's lookup is a
-- per-request cache hit on apps[host] so this index is for the
-- operator "which apps have per-route observability?" query path
-- (dashboard), not the hot request path. Mirrors 00155.
-- +goose StatementBegin
ALTER TABLE apps
    ADD COLUMN IF NOT EXISTS route_metrics_enabled boolean NOT NULL DEFAULT false;
CREATE INDEX IF NOT EXISTS apps_route_metrics_enabled_idx
    ON apps(route_metrics_enabled)
    WHERE route_metrics_enabled = true;
-- +goose StatementEnd

-- +goose Down
-- Reverse: drop the partial index, then drop the column. A row
-- that had route_metrics_enabled=true loses the bit on downgrade;
-- the GET /v1/apps/{slug} response shape omits route_metrics_enabled
-- because the column no longer exists, which is the correct
-- degraded behaviour (every app falls back to the {app, class}
-- histogram — the per-route series and the control-listener
-- reader go silent).
-- +goose StatementBegin
DROP INDEX IF EXISTS apps_route_metrics_enabled_idx;
ALTER TABLE apps
    DROP COLUMN IF EXISTS route_metrics_enabled;
-- +goose StatementEnd
