-- filename: 00412_status_incidents.sql
--
-- Issue #599 / ADR-130 / cluster D commit 14 of the
-- platform-observability mega-PR.
--
-- Status-incidents table that the public status page reads
-- from. The page (deploy/statuspage/index.html) fetches
-- /v1/internal/slo.json on load; the endpoint reads from this
-- table for active incidents and from meterd's loopback
-- Prometheus exporter for the SLO ratios (api_availability,
-- wake_latency p95, build_success ratio).
--
-- Columns:
--   id           BIGSERIAL PRIMARY KEY
--              — surrogate key for the gregalectl operator CLI
--                (`gregale status incident resolve <id>`).
--   component    TEXT NOT NULL
--              — closed-set vocabulary (apid, schedd, vmmd,
--                gatewayd, meterd, imaged, builderd,
--                faas-control-plane). Enforced by CHECK at
--                the SQL layer so a typo at the CLI surface
--                fails closed at INSERT time (23514).
--   severity     TEXT NOT NULL
--              — closed-set vocabulary (degraded, partial_outage,
--                full_outage, maintenance). Same CHECK posture.
--   message      TEXT NOT NULL
--              — operator-authored free-text message shown
--                verbatim on the status page. Length-bounded
--                to 1024 chars via CHECK so a paste of a
--                50 KB stack trace can't bloat the response.
--   posted_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
--              — when the operator opened the incident. The
--                status page renders "since <posted_at>".
--   resolved_at  TIMESTAMPTZ NULL
--              — when the operator closed the incident. NULL
--                means open. Partial index on the open rows
--                below for the "list open incidents" query.
--
-- Index:
--   status_incidents_open ON status_incidents(component) WHERE
--      resolved_at IS NULL — partial index for the hot path
--      (gatewayd-internal /v1/internal/slo.json reads the
--      open subset on every status-page load). Closed
--      incidents stay in the table for audit but don't bloat
--      the working set.
--
-- Replay-safety: every CREATE / ALTER uses IF NOT EXISTS
-- guards so a partial-apply replay (lost goose row, re-run
-- MigrateUp) is idempotent. Same convention as 00411
-- (deployments_liveness_restart_count) and 00264
-- (deployments_secret_findings).

-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS status_incidents (
    id          BIGSERIAL    PRIMARY KEY,
    component   TEXT         NOT NULL,
    severity    TEXT         NOT NULL,
    message     TEXT         NOT NULL,
    posted_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    resolved_at TIMESTAMPTZ  NULL
);

ALTER TABLE status_incidents
    DROP CONSTRAINT IF EXISTS status_incidents_component_chk;
ALTER TABLE status_incidents
    ADD  CONSTRAINT status_incidents_component_chk
        CHECK (component IN (
            'apid'::text,
            'schedd'::text,
            'vmmd'::text,
            'gatewayd'::text,
            'meterd'::text,
            'imaged'::text,
            'builderd'::text,
            'faas-control-plane'::text
        ));

ALTER TABLE status_incidents
    DROP CONSTRAINT IF EXISTS status_incidents_severity_chk;
ALTER TABLE status_incidents
    ADD  CONSTRAINT status_incidents_severity_chk
        CHECK (severity IN (
            'degraded'::text,
            'partial_outage'::text,
            'full_outage'::text,
            'maintenance'::text
        ));

ALTER TABLE status_incidents
    DROP CONSTRAINT IF EXISTS status_incidents_message_len_chk;
ALTER TABLE status_incidents
    ADD  CONSTRAINT status_incidents_message_len_chk
        CHECK (length(message) <= 1024);

CREATE INDEX IF NOT EXISTS status_incidents_open
    ON status_incidents(component)
    WHERE resolved_at IS NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS status_incidents_open;
ALTER TABLE status_incidents
    DROP CONSTRAINT IF EXISTS status_incidents_message_len_chk;
ALTER TABLE status_incidents
    DROP CONSTRAINT IF EXISTS status_incidents_severity_chk;
ALTER TABLE status_incidents
    DROP CONSTRAINT IF EXISTS status_incidents_component_chk;
DROP TABLE IF EXISTS status_incidents;
-- +goose StatementEnd
