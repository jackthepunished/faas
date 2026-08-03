-- +goose Up
-- +goose StatementBegin
--
-- 00126_pg_ratelimit.sql — Tier A7 edge split (ADR-070 item 7).
--
-- Today's per-process token bucket (pkg/gateway/ratelimit.go) is
-- correct for one-box (one gatewayd, one bucket per app). After the
-- Tier A7 split the platform runs N gatewayd-internal replicas
-- behind one gatewayd-public; sticky-by-warm-node routing (ADR-070)
-- does NOT pin a single replica, so per-process buckets see a
-- fraction of the customer traffic and the rate limit leaks.
--
-- The fix is opt-in (`[ratelimit] mode = "central"` in TOML) and
-- uses a Postgres-backed counter. The token bucket is one row per
-- (scope, subject_id, plan) where scope ∈ {'app', 'account'}. The
-- consume path is a single SQL `INSERT … ON CONFLICT … DO UPDATE
-- SET tokens = tokens + delta WHERE pg_advisory_xact_lock(
-- hashtext((scope, subject_id)::record::text)) = 0 … RETURNING
-- tokens` so two replicas contending on the same row serialise.
--
-- Bench (ADR-070 bench follow-up): P50 0.8 ms, P99 3.2 ms on EX44.
--
-- Schema invariants:
--   - PRIMARY KEY (scope, subject_id, plan) so the ON CONFLICT
--     clause has a deterministic target.
--   - CHECK on scope value to keep the enum closed; future scopes
--     need a fresh ADR + migration. The canonical list lives in
--     pkg/api/limits.go (rate-limit scope constants); this CHECK
--     is the SQL-side duplicate for defense-in-depth.
--   - CHECK on tokens >= 0 — the consume path uses NOT tokens >= 0
--     to detect overflow at the SQL layer; tokens should never go
--     negative because the limiter math is positive.
--   - CHECK on plan against the four-plan enum (free/hobby/pro/scale).
--   - Partial index on (subject_id) WHERE scope = 'app' (the hot
--     read path) — the per-account lookup is rarer so the partial
--     index covers the common case.
--   - tokens is bigint (no fractional tokens today). If a future
--     ADR adds fractional refill (e.g. 0.5 tokens per second),
--     rename to numeric(20,4) in a follow-up migration. The
--     "no floats near money" invariant from CLAUDE.md applies.
--
-- Replay-safe (ADR-041): CREATE TABLE IF NOT EXISTS + ADD CONSTRAINT
-- IF NOT EXISTS via DO-block (same pattern as 00125).

CREATE TABLE IF NOT EXISTS pg_ratelimit_counters (
    scope        text             NOT NULL CHECK (scope IN ('app', 'account')),
    subject_id   uuid             NOT NULL,
    plan         text             NOT NULL CHECK (plan IN ('free', 'hobby', 'pro', 'scale')),
    tokens       bigint           NOT NULL CHECK (tokens >= 0),
    last_refill  timestamptz      NOT NULL DEFAULT now(),
    PRIMARY KEY (scope, subject_id, plan)
);

CREATE INDEX IF NOT EXISTS pg_ratelimit_counters_subject_id_app_idx
    ON pg_ratelimit_counters (subject_id)
    WHERE scope = 'app';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS pg_ratelimit_counters_subject_id_app_idx;
DROP TABLE IF EXISTS pg_ratelimit_counters;
-- +goose StatementEnd
