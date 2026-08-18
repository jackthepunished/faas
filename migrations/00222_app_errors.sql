-- filename: 00222_app_errors.sql
-- +goose Up
-- +goose StatementBegin

-- 00222_app_errors.sql — ADR-096 customer-facing automatic error
-- grouping. Two new tables:
--
--   app_errors           — one row per (account_id, app_id,
--                          fingerprint) within the dedupe window.
--                          The grouped view.
--   app_error_requests   — one row per request that hit the
--                          grouped fingerprint. The drill-down
--                          view.
--
-- Both tables are written by gatewayd-internal over a new unix-
-- socket gRPC method (pkg/apidgrpc/apperrors.proto's
-- IncrementAppError); apid is the only writer to customer-intent
-- tables per ADR-070 / CLAUDE.md owner rules. gatewayd-internal
-- dials apid's loopback socket — it does NOT open a direct
-- Postgres connection. Pinned in ADR-096 §3.5.
--
-- Fingerprint scope (ADR-096 §3.1): tenant-app errors only. The
-- in-process classifier in cmd/gatewayd-internal/
-- app_errors_recorder.go filters out 4xx/5xx that do not resolve
-- to a customer's app slug BEFORE recording — the apid 401s,
-- admin 403s, MFA challenges are not in scope.
--
-- Slot reservation: migrations/00221_reserve_slot.sql fences
-- the prior slot per the cross-PR fence convention (the slot
-- survey on 2026-08-12 found 00215-00220 fenced in flight by
-- open PRs #849/#845/#854/#858/#851; 00221 was next free).

CREATE TABLE IF NOT EXISTS app_errors (
    id              uuid        PRIMARY KEY,
    account_id      uuid        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    app_id          uuid        NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    deployment_id   uuid            NULL REFERENCES deployments(id) ON DELETE SET NULL,
    -- 64-char lowercase hex (sha256). CHECK below enforces the
    -- length and charset so a future contributor cannot sneak an
    -- arbitrary string into the grouping key. The writer
    -- (gatewayd-internal) derives this via
    -- pkg/apidgrpc/apperrors.proto::deriveFingerprint; both ends
    -- agree on the canonical-tuple shape (ADR-096 §3.4).
    fingerprint     char(64)    NOT NULL,
    -- The matched route template (e.g. '/users/{id}'), NOT the
    -- expanded URL. Load-bearing cardinality decision (ADR-096
    -- §3.4 / Risks §10.4).
    route           text        NOT NULL,
    http_status     int         NOT NULL,
    -- Closed-vocabulary enum, enforced by CHECK below. See
    -- ADR-096 §3.2 for the allowlist.
    error_class     text        NOT NULL,
    -- 512-byte cap; the writer's redact.Redactor truncates with
    -- an ellipsis BEFORE INSERT. CHECK is the backstop in case
    -- a future writer forgets. Mirrors the half-KiB cap on
    -- log-archive partial lines.
    sample_message  text        NOT NULL CHECK (pg_column_size(sample_message) <= 512),
    -- "Issue count" — bumped on every dedupe-merge within
    -- AppErrorsDedupeWindowSeconds (default 3600). Default 1
    -- = the row was created on this write; a bump-only INSERT
    -- sets count = count + 1 and updates last_seen_at.
    count           bigint      NOT NULL DEFAULT 1,
    -- Distinct request rows that contributed (incremented by the
    -- app_error_requests INSERT path). Diverges from `count`
    -- after a dedupe-merge.
    request_count   bigint      NOT NULL DEFAULT 1,
    first_seen_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at    timestamptz NOT NULL DEFAULT now(),
    -- created_at is the row-mint time (immutable); distinct from
    -- first_seen_at which is the earliest error timestamp
    -- recorded against this fingerprint. Useful for retention
    -- purges keyed off created_at.
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT app_errors_http_status_check CHECK (http_status BETWEEN 400 AND 599),
    CONSTRAINT app_errors_fingerprint_check CHECK (fingerprint ~ '^[0-9a-f]{64}$'),
    CONSTRAINT app_errors_error_class_check CHECK (error_class IN (
        'db_timeout','stripe_timeout','null_pointer','invalid_json',
        'wake_failed','upstream_5xx','unhandled','client_error'
    ))
);

-- Summary path: top-N grouped fingerprints over a window,
-- ordered by count DESC. Index covers the primary scan; the
-- sort happens post-filter on the bounded set (limit ≤ 100).
CREATE INDEX IF NOT EXISTS app_errors_account_app_last_seen_idx
    ON app_errors (account_id, app_id, last_seen_at DESC);

-- Drill-down path: per-fingerprint scan stays narrow because
-- the fingerprint is the low-cardinality grouping key.
CREATE INDEX IF NOT EXISTS app_errors_account_app_fp_last_seen_idx
    ON app_errors (account_id, app_id, fingerprint, last_seen_at DESC);

-- ON CONFLICT tripwire for the dedupe-merge INSERT in
-- cmd/apid/grpc_server_apperrors.go. The dedupe window is
-- enforced by the writer's LRU; this unique constraint is the
-- last-resort guarantee that two writers cannot create two rows
-- for the same fingerprint.
CREATE UNIQUE INDEX IF NOT EXISTS app_errors_dedupe_uniq
    ON app_errors (account_id, app_id, fingerprint);

-- Per-request drill-down rows.
CREATE TABLE IF NOT EXISTS app_error_requests (
    id              uuid        PRIMARY KEY,
    account_id      uuid        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    app_id          uuid        NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    fingerprint     char(64)    NOT NULL,
    -- uuid v7 from the gateway's x-faas-request-id.
    request_id      uuid        NOT NULL,
    received_at     timestamptz NOT NULL,
    route           text        NOT NULL,
    http_status     int         NOT NULL,
    error_class     text        NOT NULL,
    sample_message  text        NOT NULL CHECK (pg_column_size(sample_message) <= 512),
    deployment_id   uuid            NULL REFERENCES deployments(id) ON DELETE SET NULL,
    -- Redacted subset of request headers. The writer
    -- (gatewayd-internal) caps at 8 keys and 256 bytes/value;
    -- the column CHECK caps the total jsonb size at 8 KiB so a
    -- header flood cannot bloat the row.
    headers_sample  jsonb           NULL CHECK (
        headers_sample IS NULL OR pg_column_size(headers_sample::text) <= 8192
    ),
    -- Redaction markers the writer applied (so the read path
    -- can surface the "we redacted X / Y / Z" badge).
    redactions      text[]      NOT NULL DEFAULT '{}',
    CONSTRAINT app_error_requests_http_status_check CHECK (http_status BETWEEN 400 AND 599),
    CONSTRAINT app_error_requests_fingerprint_check CHECK (fingerprint ~ '^[0-9a-f]{64}$'),
    CONSTRAINT app_error_requests_error_class_check CHECK (error_class IN (
        'db_timeout','stripe_timeout','null_pointer','invalid_json',
        'wake_failed','upstream_5xx','unhandled','client_error'
    ))
);

-- Drill-down cursor pagination: ORDER BY received_at DESC,
-- request_id DESC. Index covers the (account_id, app_id,
-- fingerprint, received_at DESC, request_id DESC) scan.
CREATE INDEX IF NOT EXISTS app_error_requests_drill_idx
    ON app_error_requests (account_id, app_id, fingerprint, received_at DESC, request_id DESC);

-- Retention purge: account-scoped nightly DELETE keyed off
-- received_at. The (account_id, received_at) btree covers the
-- DELETE where-clause; the planner picks it for
-- DeleteAppErrorRequestsOlderThan(account_id, cutoff). A
-- partial predicate ("old rows only") was rejected at
-- migration-apply time because now() is volatile (42P17:
-- functions in index predicate must be marked IMMUTABLE);
-- the plain btree is the simpler fix and the index never
-- grew large enough to need the partial trick — the
-- retention-cron spends most of its run time on inserts,
-- not on retention reads. 90-day floor matches the Scale
-- plan's AppErrorsRetentionDays
-- (cmd/apid/app_errors_purge.go).
CREATE INDEX IF NOT EXISTS app_error_requests_retention_idx
    ON app_error_requests (account_id, received_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS app_error_requests;
DROP TABLE IF EXISTS app_errors;

-- +goose StatementEnd