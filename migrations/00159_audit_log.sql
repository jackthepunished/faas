-- filename: 00159_audit_log.sql
-- +goose Up
-- Issue #755 / PR-5: audit_log table for events-FK-survival across
-- account deletion. Mirrors the AWS CloudTrail immutable event
-- history pattern: the audit row outlives the account it relates
-- to so a DPO / regulator can re-derive the post-deletion state.
--
-- Shape: a denormalised copy of the events.kind row that has
-- (account_id NULLABLE) and no FK to accounts. account_email is
-- captured at copy-time so an auditor reading the post-deletion
-- row has the human identifier without joining back to a deleted
-- account row. data is the verbatim jsonb payload the auditor
-- wrote at emit time.
--
-- Populated by the existing events → audit_log backfill inside the
-- DeleteAccount store method (pkg/state/pgstore.go::DeleteAccount).
-- Read-only by spec: no UPDATE / DELETE on this table; only INSERT
-- from the backfill path.
--
-- Index: (received_at desc) is the dashboard-default sort order.
-- (account_id, received_at desc) supports the rare post-deletion
-- "show me everything about this account_id" query a DPO might run.

CREATE TABLE IF NOT EXISTS audit_log (
    id              UUID PRIMARY KEY,
    kind            TEXT        NOT NULL,
    account_id      UUID,                          -- nullable; no FK to accounts (survives deletion)
    account_email   TEXT,                          -- captured at copy-time so the audit row is self-contained
    actor           TEXT,
    received_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    data            JSONB
);

CREATE INDEX IF NOT EXISTS audit_log_received_at_idx
    ON audit_log (received_at DESC);

CREATE INDEX IF NOT EXISTS audit_log_account_idx
    ON audit_log (account_id, received_at DESC)
    WHERE account_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS audit_log_account_idx;
DROP INDEX IF EXISTS audit_log_received_at_idx;
DROP TABLE IF EXISTS audit_log;