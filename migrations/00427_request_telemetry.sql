-- filename: 00427_request_telemetry.sql
-- +goose Up
-- +goose StatementBegin
--
-- ADR-127 — production debugger per-request data plane.
-- One row per gateway-served request: status, latency, route, app,
-- deployment, cold-boot flag, W3C trace_id, and (post-§3 amendment)
-- customer OTel span summary. The data plane that makes the example
-- insight ("POST /checkout became 38% slower after v81; PostgreSQL
-- queries 82ms → 191ms; 31% affected; started 18:42 UTC; [Compare v80]
-- [Replay] [Rollback]") derivable end-to-end.
--
-- Why a dedicated table and not the existing `usage_minutes` or
-- `mirror_invocation_results`:
--   * usage_minutes (pkg/meter/sampler.go:441) stores mb_seconds +
--     requests + cpu_usec + tx_bytes + cold_boot_count, keyed by
--     (instance_id, minute). No latency column, no per-deployment
--     split. Cannot answer "did v81 make it slow?".
--   * mirror_invocation_results (migrations/00386_) only fires when
--     the customer opted into an explicit mirror rule (ADR-125). The
--     99% of requests that aren't mirrored are exactly the ones the
--     operator needs to debug.
--
-- Column semantics:
--
--   * id, account_id, app_id, deployment_id — the join keys for
--     IDOR-safe retrieval. account_id is denormalised off apps so the
--     regression endpoint doesn't need a 3-table join per request
--     (same denormalisation pattern as 00386_:25-37).
--   * route — text. The route template (e.g. `GET /checkout/{id}`),
--     NOT the literal URL — the cardinality guardrail that keeps the
--     Prometheus histogram and the per-route view bounded. Stamped at
--     `pkg/gateway/handler.go:4533-4534` from the `routeTemplateKey`
--     context value.
--   * method — text. Closed enum: GET/POST/PUT/PATCH/DELETE/HEAD/OPTIONS.
--   * status — int. HTTP status, 100-599 (CHECK).
--   * latency_ms — int. Wall-clock from Handler.ServeHTTP entry to
--     Handler.observe exit. Same clock domain as the existing
--     gateway_request_duration_seconds histogram so the two stay
--     reconcilable.
--   * cold_boot — boolean. True when the request woke a fresh
--     instance (snapshot restore or cold boot). Read at observe time
--     from the existing cold flag passed to observe (handler.go:5516).
--   * trace_id — text. W3C trace-id hex (32 chars). NULL when the
--     request had no W3C context (rare — gateway always sets one).
--   * spans_summary — jsonb. Populated by ADR-127 §3 customer OTel
--     ingest: top-N slowest child spans + DB-statement attributes.
--     Default NULL; PR-A ships the column, PR-A §5 ships the writer.
--   * received_at — timestamptz. Defaults to now() at insert;
--     gateway stamps this from the same clock it stamps the
--     `gateway_request_duration_seconds` histogram, so the histogram
--     bucket count and the row count stay within the same microsecond.
--
-- Partitioning posture:
--   * PARTITION BY RANGE (received_at) — monthly partitions keep
--     index size bounded. Primary key is (id, received_at) to
--     satisfy Postgres's rule that partition keys belong to the PK.
--   * Default partition catches anything outside the explicit
--     current/next-month partitions. The PR-B cron sweep drops the
--     default partition once all its rows are past the per-plan
--     retention cap; PR-A does not own the cron.
--
-- Indexing posture:
--   * request_telemetry_app_received_idx on (app_id, received_at DESC)
--     — the canonical read pattern: "give me the last N requests for
--     this app". Matches mirror_invocation_results_rule_time_idx's
--     DESC ordering (00386_:119-120).
--   * request_telemetry_app_dep_received_idx on (app_id,
--     deployment_id, received_at DESC) — the regression detection
--     pattern: "give me p95 per route for deployment X, then compare
--     to deployment Y".
--   * request_telemetry_trace_idx on (trace_id) WHERE trace_id IS
--     NOT NULL — partial index, only non-NULL trace_ids. The
--     customer-OTel join pattern: "given a span trace_id, find the
--     request row".
--
-- Retention posture:
--   * Per-plan cap via DebugTelemetryRetentionDays (limits.go,
--     ADR-127 §3). Hobby=3, Pro=7, Scale=14, Free=0 (off). Enforced
--     by RetentionOnceRequestTelemetry in pkg/meter/retention.go,
--     wired from cmd/meterd/main.go:926-928. The sweep drops the
--     oldest monthly partition whose max(received_at) < now() - cap.

-- Replay-safe posture: every CREATE in this Up block uses IF NOT EXISTS
-- (same pattern 00386_ uses, lines 90-93). TestNewMigrationsAreReplaySafe
-- replays each new migration on a fresh DB; without IF NOT EXISTS the
-- second run fails with 42P07 relation-already-exists.
CREATE TABLE IF NOT EXISTS request_telemetry (
    id              uuid         DEFAULT gen_random_uuid(),
    account_id      uuid         NOT NULL,
    app_id          uuid         NOT NULL,
    deployment_id   uuid         NOT NULL,
    -- route is the closed-enum route label admitted via routeLabelSet
    -- (pkg/gateway/handler.go:4613-4626). The 256-char cap mirrors the
    -- longest routeLabelSet admit format: method(3-7) + space +
    -- path(<=248) — the bound prevents a misconfigured / hostile
    -- upstream from blowing up the index with arbitrarily long strings.
    route           text         NOT NULL CHECK (length(route) BETWEEN 1 AND 256),
    -- method is the closed HTTP verb enum. Anything outside the set
    -- is a writer bug — the recorder never emits verbs outside the
    -- set; the CHECK is the last line of defense before the row lands
    -- in the index.
    method          text         NOT NULL CHECK (method IN ('GET','POST','PUT','PATCH','DELETE','HEAD','OPTIONS')),
    status          int          NOT NULL CHECK (status BETWEEN 100 AND 599),
    latency_ms      int          NOT NULL CHECK (latency_ms >= 0),
    cold_boot       boolean      NOT NULL DEFAULT false,
    trace_id        text         CHECK (trace_id IS NULL OR trace_id ~ '^[0-9a-f]{32}$'),
    spans_summary   jsonb,
    received_at     timestamptz  NOT NULL DEFAULT now(),
    PRIMARY KEY (id, received_at)
) PARTITION BY RANGE (received_at);

CREATE TABLE IF NOT EXISTS request_telemetry_default
    PARTITION OF request_telemetry DEFAULT;

CREATE INDEX IF NOT EXISTS request_telemetry_app_received_idx
    ON request_telemetry (app_id, received_at DESC);
CREATE INDEX IF NOT EXISTS request_telemetry_app_dep_received_idx
    ON request_telemetry (app_id, deployment_id, received_at DESC);
CREATE INDEX IF NOT EXISTS request_telemetry_trace_idx
    ON request_telemetry (trace_id)
    WHERE trace_id IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS request_telemetry_trace_idx;
DROP INDEX IF EXISTS request_telemetry_app_dep_received_idx;
DROP INDEX IF EXISTS request_telemetry_app_received_idx;
DROP TABLE IF EXISTS request_telemetry;
-- +goose StatementEnd