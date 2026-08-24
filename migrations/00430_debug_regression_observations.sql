-- filename: 00430_debug_regression_observations.sql
-- +goose Up
-- +goose StatementBegin

-- ADR-127 §PR-B — persist regression observations.
--
-- The apid regression cron (cmd/apid/debug_regression_cron.go)
-- runs every 5 minutes. For each active deployment of each app
-- with non-empty telemetry, it computes:
--   1. The p95 latency baseline from the prior deployment via
--      RequestTelemetryBaselineP95ByRoute (pkg/state/queries.sql:1585-1598).
--   2. The set of requests in the current deployment whose
--      latency exceeds p95_base * 1.20 (ADR-127 §Decision 5).
--   3. If the affected_count exceeds DebugRegressionMinAffected
--      (per-plan, defaults at PR-B's wire-up), it persists an
--      upserted row keyed by (app_id, deployment_id, route).
--
-- Why persist (vs ephemeral in-memory state):
--   * Dashboard UX "X regressions since deploy" requires
--     persistence — the rendered HTML needs last_detected_at
--     to be a column.
--   * Cross-restart durability: an apid restart loses the in-
--     memory detector state; the cron is the only thing that
--     rebuilds it. Without persistence, a restart blanks the
--     regression banner for up to one 5-minute cron tick.
--   * Cross-instance observability: in a future Tier A multi-
--     host control plane, multiple apid instances each run the
--     cron; the UPSERT semantics (PRIMARY KEY conflict) keep
--     the row count bounded.
--
-- Schema rationale (mirrors domain_doctor_observations at
-- migrations/00313 with regression-specific fields):
--
--   * PRIMARY KEY (app_id, deployment_id, route). The cron
--     upserts on this triple so the table grows at most one
--     row per (deployment, route) — not one row per cron
--     tick. Matches UpsertDoctorObservation's primary-key
--     upsert shape.
--   * p95_ms / p95_base_ms — the absolute and baseline p95
--     for the route in the affected window. Both CHECK >= 0.
--   * affected_count — INT NOT NULL. The number of requests
--     in the deployment whose latency exceeded p95_base *
--     1.20 during the window. CHECK >= 0.
--   * regression_factor — NUMERIC(5,2). p95_ms / p95_base_ms.
--     Stored so the dashboard sorts / alerts without re-
--     computing. CHECK >= 1.0 (a "regression" is by definition
--     slower than baseline; values < 1.0 mean the deployment
--     is faster, which the cron does NOT write — the cron
--     only fires when latency > baseline).
--   * first_detected_at — set on INSERT. Survives subsequent
--     upserts because the upsert UPDATE clause does NOT
--     touch this column. Renders "regression detected 4h ago".
--   * last_detected_at — refreshed to now() on every upsert.
--     Renders "last seen 2 minutes ago" and feeds the
--     since=<duration> filter on the dashboard and the
--     GET /v1/apps/{slug}/debug/regressions endpoint.
--
-- Indexing posture:
--   * debug_regression_observations_app_idx on (app_id,
--     last_detected_at DESC). The dashboard read pattern:
--     "give me the latest regressions for this app". DESC
--     ordering matches request_telemetry_app_received_idx at
--     00427:117 so query plans read in the same direction.
--
-- Replay-safe posture: CREATE TABLE / CREATE INDEX use IF
-- NOT EXISTS (canonical pattern from 00053 + 00287 + 00313).
-- TestNewMigrationsAreReplaySafe pins the second pass as a
-- no-op.

CREATE TABLE IF NOT EXISTS debug_regression_observations (
    app_id              uuid         NOT NULL,
    deployment_id       uuid         NOT NULL,
    -- route mirrors the closed-enum route label admitted via
    -- routeLabelSet (pkg/gateway/handler.go:4613-4626) and the
    -- request_telemetry.route CHECK at 00427:98 — same 256-char
    -- cap so the regression cron can join on route verbatim.
    route               text         NOT NULL CHECK (length(route) BETWEEN 1 AND 256),
    p95_ms              int          NOT NULL CHECK (p95_ms >= 0),
    p95_base_ms         int          NOT NULL CHECK (p95_base_ms >= 0),
    affected_count      int          NOT NULL CHECK (affected_count >= 0),
    regression_factor   numeric(5,2) NOT NULL CHECK (regression_factor >= 1.0),
    first_detected_at   timestamptz  NOT NULL DEFAULT now(),
    last_detected_at    timestamptz  NOT NULL DEFAULT now(),
    PRIMARY KEY (app_id, deployment_id, route)
);

CREATE INDEX IF NOT EXISTS debug_regression_observations_app_idx
    ON debug_regression_observations (app_id, last_detected_at DESC);

-- +goose StatementEnd

-- +goose Down
-- Forward-only by design (mirrors 00313:54-73 + 00287 + 00229):
-- reverting would orphan rows that the cron wrote between the
-- apply and the rollback. Down is a sentinel SELECT 1 so a
-- replay lands on the CREATE, not on a destructive DROP.
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd