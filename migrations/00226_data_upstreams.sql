-- filename: 00226_data_upstreams.sql
-- +goose Up
-- +goose StatementBegin

-- 00226_data_upstreams.sql — ADR-098 connection-aware execution
-- (§9.A / spec §9.A). Two new tables:
--
--   data_upstreams       — one row per (account_id, app_id,
--                          scope, kind, host, port) the apid
--                          env-classifier has recorded for an
--                          app. Source = 'inferred' (from env
--                          value, ADR-098 D1.a) or 'explicit'
--                          (POST .../upstreams, ADR-098 D4).
--                          Dedupe key: (app_id, scope, kind,
--                          host, port) with ON CONFLICT
--                          DO UPDATE.
--   data_upstream_probes — sliding 30s × 5min TCP+TLS probe
--                          samples (ADR-098 D2). Written by
--                          meterd's probe loop; read by schedd
--                          on wake to seed PreferredRegion
--                          (ADR-098 D3).
--
-- Ownership: apid is the ONLY writer to data_upstreams
-- (env-classifier side) — mirrors the apps/app_envs
-- owner rule (CLAUDE.md); meterd is the ONLY writer to
-- data_upstream_probes. Both are read by schedd via the
-- Store interface. pg_notify on data_upstreams (channel
-- `data_upstreams_changed`) is the wake-side hint: schedd
-- subscribes on a per-account LISTEN, the payload is
-- (app_id, scope, kind, host, port, op).
--
-- Default-OFF per ADR-098 D7: no production code path
-- reads these tables until PR-B wires the customer
-- surface and the meterd probe loop. PR-A's only
-- runtime effect is the table DDL + the trigger.
--
-- Slot reservation: 00226_reserve_slot.sql (PR-0 / PR
-- #858, MERGED 2026-08-13) fenced the slot. This file
-- replaces the fence per ADR-041's slot reservation
-- convention. Sibling PRs #864 (edge-rules-kind-budget
-- fence), #867, and #845 each carry their own
-- 00226_reserve_slot.sql fences; once PR-A lands on
-- main, those siblings must drop their fences on
-- rebase — `fix(migration): drop 00226_reserve_slot.sql`
-- is the canonical renumber playbook commit (ADR-098
-- §renumber + ADR-041).
--
-- Cross-PR slot gate carve-out
-- (`scripts/ci/check_migration_slots.sh::slots_from_paths`
-- regex
-- `'^migrations/[0-9]{5}_(.*_)?(reservation|reserve_slot)(_[^/]*)?\.sql$'`)
-- hides reservation filenames from the collision check.
-- Once PR-A merges, the regex no longer hides this file
-- (the basename is `00226_data_upstreams.sql`, not a
-- reservation); the gate will now enforce 226 as a real
-- slot. That's intentional: the fence-to-real move is
-- the moment slot ownership consolidates.
--
-- Replay safety: the migration uses `CREATE TABLE IF NOT
-- EXISTS`, `CREATE INDEX IF NOT EXISTS`, `CREATE UNIQUE
-- INDEX IF NOT EXISTS`, `DROP TRIGGER IF EXISTS` +
-- `CREATE TRIGGER` (drop-before-create per the
-- trigger-replay-safety-drop-before-create memory), and
-- a partitioned table whose `PARTITION OF … DEFAULT`
-- child is itself idempotent. The replay-safety harness
-- (TestNewMigrationsAreReplaySafe in
-- migrations/replay_safety_test.go) applies the
-- migration twice in a single tx and pins the second
-- pass as a no-op.

----------------------------------------------------------------------
-- data_upstreams (ADR-098 D1.a — capture, D4 — customer surface)
----------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS data_upstreams (
    id              uuid        PRIMARY KEY,
    account_id      uuid        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    app_id          uuid        NOT NULL REFERENCES apps(id)      ON DELETE CASCADE,
    -- 'inferred' = apid env-classifier recorded this from
    -- the customer's env (ADR-098 D1.a, plaintext values
    -- NEVER leave the handler). 'explicit' = POST
    -- /v1/apps/{slug}/upstreams (ADR-098 D4, PR-B). The
    -- distinction lets the dashboard render "you didn't
    -- set this — we inferred it from DATABASE_URL" badges.
    source          text        NOT NULL,
    -- 'default' / 'staging' / 'prod' / ... — mirrors
    -- app_envs.scope + deployments.scope (ADR-091). The
    -- shape regex matches the existing
    -- `app_envs_scope_shape` / `deployments_scope_shape`
    -- precedent (3..40 chars, [a-z0-9-]).
    scope           text        NOT NULL,
    -- Closed vocabulary — ADR-098 D1. Fourteen kinds.
    -- The CHECK below is the wire-bypass backstop; the
    -- apid classifier fails fast on an unknown kind via
    -- api.UpstreamKindIsValid() (PR-B).
    kind            text        NOT NULL,
    -- Plaintext host extracted via net/url.Parse from the
    -- env value (ADR-098 D1.b). Inspection surface only:
    -- the dashboard renders this in the inferred list and
    -- the operator console renders it for §11 review.
    -- Never used as a routing key — Prom labels and the
    -- schedd affinity map key on host_redacted_hash.
    host            text        NOT NULL,
    -- TCP port (1..65535). 0 is rejected by CHECK; an
    -- env value without a port parses to the protocol
    -- default (postgres=5432, redis=6379, ...) and the
    -- classifier stamps the resolved value (PR-B).
    port            int         NOT NULL,
    -- sha256(salt + host) where salt comes from
    -- pkg/secretbox (deploy-time secret, ADR-098 D6).
    -- NOT NULL — the writer (PR-B's
    -- cmd/apid/extract.go) generates the hash before
    -- INSERT. The sentinel '__unsalted__' is accepted
    -- for test/example rows only (test fixtures in
    -- pkg/e2etest and the migration-apply test below);
    -- a future production writer must never stamp the
    -- sentinel. See the quiescence-grep step in the
    -- PR-A plan (`git grep -nF '__unsalted__' ...`).
    host_redacted_hash text     NOT NULL,
    -- Operator-declared region (free-text, 'us-east-1',
    -- 'eu-central-1', 'auto', ...). NULL until the
    -- classifier derives it from the host suffix
    -- (e.g. `.us-east-2.aws.neon.tech` → 'us-east-2')
    -- or the customer sets it via POST .../upstreams.
    declared_region text            NULL,
    -- Last TCP+TLS probe RTT observed for this host.
    -- Denormalised here so the dashboard's inferred-list
    -- render doesn't have to JOIN data_upstream_probes
    -- on every read. NULL means "never probed" (the
    -- classifier wrote the row before meterd's first
    -- sample landed). Updated by meterd's probe loop on
    -- every successful sample; CHECK bounds below zero.
    last_rtt_ms     int             NULL,
    last_probed_at  timestamptz     NULL,
    -- Dedupe key: (app_id, scope, kind, host, port). The
    -- ON CONFLICT clause in pkg/state/queries.sql's
    -- InsertDataUpstream targets this UNIQUE — a second
    -- inferred row from a redeploy bumps last_seen_at +
    -- last_probed_at + last_rtt_ms; an explicit POST
    -- upserts. ON CONFLICT tripwire mirrors the
    -- app_errors_dedupe_uniq precedent (migrations
    -- /00222_app_errors.sql).
    last_seen_at    timestamptz NOT NULL DEFAULT now(),
    -- created_at is the row-mint time (immutable);
    -- distinct from last_seen_at (recency of any
    -- observation). Useful for retention purges keyed
    -- off created_at + a customer's app-soft-delete
    -- cascade.
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT data_upstreams_source_check
        CHECK (source IN ('inferred','explicit')),
    CONSTRAINT data_upstreams_scope_check
        -- Mirror `app_envs_scope_shape` (migrations
        -- /00203_app_envs_scope.sql) +
        -- `deployments_scope_shape` (migrations
        -- /00213_deployments_scope.sql): 3..40 chars,
        -- [a-z0-9-], no leading/trailing dash.
        CHECK (scope ~ '^[a-z0-9]([a-z0-9-]{1,38})[a-z0-9]$'),
    CONSTRAINT data_upstreams_kind_check
        CHECK (kind IN (
            'postgres','redis','mongo','cassandra',
            'clickhouse','elasticsearch','opensearch',
            'rabbitmq','kafka','nats','minio',
            'memcached','etcd','s3','https_api'
        )),
    CONSTRAINT data_upstreams_host_check
        -- RFC 1123 hostname: labels of [a-z0-9-]{1,63}
        -- joined by dots, total <= 253 chars. Wildcard
        -- hosts (e.g. *.s3.amazonaws.com), hosts with
        -- underscores or other non-RFC characters, AND
        -- IPv4 literals (e.g. 192.168.1.1, 127.0.0.1)
        -- are explicitly rejected here — the classifier
        -- normalises those shapes BEFORE INSERT, so
        -- anything that lands in the DB matches the
        -- strict regex. The IPv4 backstop is the second
        -- conjunct: `^[0-9]+(\.[0-9]+)+$` matches a
        -- host whose every label is all digits separated
        -- by dots (i.e. an IPv4 dotted-quad literal);
        -- the NOT gates it off, so an IPv4 that snuck
        -- through the first regex (e.g. via Postgres's
        -- ARE allowing `192.168.1.1` as four [a-z0-9]
        -- labels) still trips this CHECK (23514)
        -- before the row poisons the probe loop.
        CHECK (host ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$'
               AND host !~ '^[0-9]+(\.[0-9]+)+$'
               AND length(host) BETWEEN 1 AND 253),
    CONSTRAINT data_upstreams_port_check
        CHECK (port BETWEEN 1 AND 65535),
    CONSTRAINT data_upstreams_host_redacted_hash_check
        CHECK (host_redacted_hash ~ '^[a-f0-9]{64}$'
               OR host_redacted_hash = '__unsalted__'),
    CONSTRAINT data_upstreams_declared_region_check
        CHECK (declared_region IS NULL
               OR declared_region ~ '^[a-z0-9_-]{1,32}$'),
    CONSTRAINT data_upstreams_last_rtt_ms_check
        CHECK (last_rtt_ms IS NULL
               OR (last_rtt_ms >= 0 AND last_rtt_ms <= 600000)),
    CONSTRAINT data_upstreams_last_probed_pair_chk
        CHECK ((last_rtt_ms IS NULL AND last_probed_at IS NULL)
               OR (last_rtt_ms IS NOT NULL AND last_probed_at IS NOT NULL))
);

-- Per-app listing path: GET /v1/apps/{slug}/upstreams
-- (PR-B). ORDER BY created_at DESC, id DESC. The
-- cursor pagination uses (created_at, id) — distinct
-- from app_errors' (last_seen_at, fingerprint) cursor
-- because data_upstreams' surface is a stable list,
-- not a hot recency list.
CREATE INDEX IF NOT EXISTS data_upstreams_app_created_idx
    ON data_upstreams (app_id, created_at DESC, id DESC);

-- Host-region probe lookup path: meterd's probe loop
-- reads the most-recent sample for (host, region) via
-- ListDataUpstreamProbesByHostRegion. The
-- host_redacted_hash is the lookup key (plaintext host
-- never reaches meterd — it lives at the apid side
-- only, per ADR-098 D1.b).
CREATE INDEX IF NOT EXISTS data_upstreams_host_redacted_idx
    ON data_upstreams (host_redacted_hash);

-- ON CONFLICT tripwire for the dedupe-merge INSERT in
-- pkg/state/queries.sql::InsertDataUpstream. Mirrors the
-- app_errors_dedupe_uniq pattern.
CREATE UNIQUE INDEX IF NOT EXISTS data_upstreams_dedupe_uniq
    ON data_upstreams (app_id, scope, kind, host, port);

----------------------------------------------------------------------
-- data_upstream_probes (ADR-098 D2 — probe)
--
-- PG15 declarative partitioning on sampled_at (monthly).
-- PR-A creates the parent + a DEFAULT partition (safety
-- net for late samples + tests that bypass the partition
-- creator). PR-C adds the monthly CREATE TABLE …
-- PARTITION OF cron job (out of scope for PR-A).
--
-- First partitioned table in the repo (per the schema
-- survey at PR-A plan time). The PR-A test asserts the
-- partition strategy via pg_partitioned_table.partstrat
-- = 'r' and the first partkey att = sampled_at, so any
-- future schema-dump tool that strips the PARTITION BY
-- clause trips the test before merge.
----------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS data_upstream_probes (
    id              uuid        NOT NULL,
    -- host_redacted_hash is the probe-side identifier;
    -- meterd never sees plaintext host. Mirrors
    -- data_upstreams.host_redacted_hash so the FK on
    -- this table points to a known upstreams row.
    host_redacted_hash text     NOT NULL,
    -- Region the probe was run from — the meterd
    -- node's compute_nodes.region. Mirrors the
    -- data_upstreams.declared_region vocabulary.
    region          text        NOT NULL,
    -- The probe kind for the host (postgres / redis /
    -- ...). Carried on the probe row so the schedd
    -- affinity map (pkg/sched/upstream_affinity.go,
    -- PR-B) can key on (kind, region) without
    -- joining data_upstreams.
    kind            text        NOT NULL,
    sampled_at      timestamptz NOT NULL,
    rtt_ms          int             NULL,
    -- ok = the TCP+TLS handshake completed within
    -- ProbeTimeoutMs. fail = timeout / refused / TLS
    -- handshake error / DNS failure.
    ok              bool        NOT NULL,
    -- The error class on ok=false. NULL on ok=true.
    -- Closed vocabulary: 'timeout' | 'refused' |
    -- 'tls_handshake' | 'dns' | 'unreachable'. Mirrors
    -- the invocations.error_class surface for
    -- consistency.
    error_class     text            NULL,
    -- The owning meterd node — useful for §12
    -- fleet-aggregated probe dashboards. NULL on
    -- pre-fleet rows (single-box installs).
    probe_node      text            NULL,
    CONSTRAINT data_upstream_probes_kind_check
        CHECK (kind IN (
            'postgres','redis','mongo','cassandra',
            'clickhouse','elasticsearch','opensearch',
            'rabbitmq','kafka','nats','minio',
            'memcached','etcd','s3','https_api'
        )),
    CONSTRAINT data_upstream_probes_region_check
        CHECK (region ~ '^[a-z0-9_-]{1,32}$'),
    CONSTRAINT data_upstream_probes_rtt_ms_check
        CHECK (rtt_ms IS NULL
               OR (rtt_ms >= 0 AND rtt_ms <= 600000)),
    CONSTRAINT data_upstream_probes_error_class_check
        CHECK (error_class IS NULL
               OR error_class IN ('timeout','refused','tls_handshake','dns','unreachable')),
    CONSTRAINT data_upstream_probes_ok_pair_chk
        CHECK ((ok = true  AND rtt_ms IS NOT NULL AND error_class IS NULL)
            OR (ok = false AND error_class IS NOT NULL)),
    PRIMARY KEY (id, sampled_at)
) PARTITION BY RANGE (sampled_at);

-- Default partition — safety net for late samples and
-- for test fixtures that bypass the partition creator.
-- Idempotent (CREATE TABLE IF NOT EXISTS), so the
-- replay-safety second pass finds the same shape and
-- does not error. Real partitions are managed by
-- pkg/meter/probe_partitions.go (PR-C) — out of scope
-- for PR-A.
CREATE TABLE IF NOT EXISTS data_upstream_probes_default
    PARTITION OF data_upstream_probes DEFAULT;

----------------------------------------------------------------------
-- pg_notify trigger (ADR-098 D2 consumer side)
--
-- Channel `data_upstreams_changed` — payload
-- `(app_id|scope|kind|host|port|op)`. schedd subscribes
-- per-account via LISTEN in pkg/sched/listen.go (PR-B);
-- the wake-side handler refreshes the in-process
-- upstream-affinity map (pkg/sched/upstream_affinity.go)
-- when an INSERT/UPDATE/DELETE fires.
--
-- Drop-before-create per the
-- trigger-replay-safety-drop-before-create memory: the
-- second replay pass would otherwise hit
-- SQLSTATE 42P07 (trigger … already exists). DROP
-- IF EXISTS makes the replay no-op.
--
-- Pipe-delimited payload (rather than the JSONB format
-- github_webhook_secrets uses) keeps the format
-- string under the 8000-byte pg_notify limit even on a
-- worst-case 253-char host, and lets the consumer
-- parse it without a JSON dep on the receive path.
----------------------------------------------------------------------

DROP TRIGGER IF EXISTS data_upstreams_notify_trg ON data_upstreams;

-- Pipe-delimited payload:
--   app_id|scope|kind|host|port|op
-- On DELETE, NEW is NULL — fall back to OLD so the
-- subscriber (schedd's pkg/sched/upstream_affinity.go,
-- PR-B) can still identify the row by (app_id, scope,
-- kind, host, port). The four row-identity fields are
-- stable across the row lifecycle; subscribers index
-- their in-process map keyed on (app_id, scope, kind,
-- host, port) and need them on every op.
CREATE OR REPLACE FUNCTION data_upstreams_notify() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify(
        'data_upstreams_changed',
        format('%s|%s|%s|%s|%s|%s',
            COALESCE(NEW.app_id, OLD.app_id)::text,
            COALESCE(NEW.scope, OLD.scope),
            COALESCE(NEW.kind, OLD.kind),
            COALESCE(NEW.host, OLD.host),
            COALESCE(NEW.port, OLD.port)::text,
            TG_OP)
    );
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER data_upstreams_notify_trg
    AFTER INSERT OR UPDATE OR DELETE ON data_upstreams
    FOR EACH ROW
    EXECUTE FUNCTION data_upstreams_notify();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TRIGGER IF EXISTS data_upstreams_notify_trg ON data_upstreams;
DROP FUNCTION IF EXISTS data_upstreams_notify();
DROP TABLE IF EXISTS data_upstream_probes_default;
DROP TABLE IF EXISTS data_upstream_probes;
DROP TABLE IF EXISTS data_upstreams;

-- +goose StatementEnd
