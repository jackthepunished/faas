-- +goose Up
-- +goose StatementBegin
--
-- 00116_warm_hint.sql — Tier A7 edge split (ADR-070).
--
-- The sticky-warm hint that gatewayd reads to bias per-app routing
-- toward the node that most recently warmed the app used to live
-- only on the per-process WarmHintCache (pkg/gateway/warmhint_cache.go),
-- fed by schedd's StreamWarmHints gRPC. After the gatewayd-public /
-- gatewayd-internal split, both daemons want the same hint and they
-- live on different processes (and, in multi-host mode, on different
-- boxes). gRPC fan-out would double the stream count per box and
-- (worse) the public daemon would depend on every schedd peer for
-- cold-miss lookups.
--
-- Instead the warm hint becomes a tiny Postgres-backed publication:
--
--   1. New single-row-per-app table `warm_hint` carrying the most
--      recent (app_id, node_id) stamp.
--   2. Schedd writes via INSERT … ON CONFLICT (app_id) DO UPDATE on
--      every successful emit (pkg/sched/warmhint.go::Broadcaster).
--   3. Each write fires a pg_notify on the new channel
--      `warm_hint_published` so gatewayd-public and gatewayd-internal
--      can mirror the row in their own LRU caches without polling.
--
-- Schema invariants:
--   - PRIMARY KEY (app_id) so the ON CONFLICT clause serialises
--     concurrent writers at the row level (PG row lock).
--   - written_at is NOT NULL with DEFAULT now() — there is no
--     "partial write" state for this table.
--   - CHECK (written_at <= now() + interval '1 minute') to keep
--     clock skew from poisoning the cache (same pattern as
--     migrations/00091_instances_migrated_at.sql:191-193).
--   - Index on (node_id) supports future "list all apps warm on
--     node X" queries (operator dashboard); see operator-ops.md
--     §warm-hint-bulk-lookup for the use case.
--
-- Replay-safe (ADR-041): CREATE TABLE IF NOT EXISTS + DO-block-guarded
-- ADD CONSTRAINT (matching migrations/00082_apps_scaling_policy.sql:50-77).
-- A second MigrateUp is a no-op.
--
-- Wire: pkg/db/notify.go::NotifyWarmHintPublished = "warm_hint_published".
-- Consumers (this PR cluster wires the public side; the file-move
-- cluster wires the internal side):
--   - cmd/gatewayd-public/warmhint_cache.go (NEW)
--   - cmd/gatewayd-internal/warmhints.go (NEW, moved from
--     cmd/gatewayd/warmhints.go)

CREATE TABLE IF NOT EXISTS warm_hint (
    app_id     uuid        PRIMARY KEY,
    node_id    uuid        NOT NULL,
    written_at timestamptz NOT NULL DEFAULT now()
);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conname = 'warm_hint_written_at_chk'
          AND conrelid = 'warm_hint'::regclass
    ) THEN
        ALTER TABLE warm_hint
            ADD CONSTRAINT warm_hint_written_at_chk
            CHECK (written_at <= now() + interval '1 minute');
    END IF;
END$$;

-- Index on node_id for the future "list all hot apps on node X"
-- dashboard query. node_id is NOT NULL, so the WHERE clause would
-- be dead — a plain index is the right shape.
CREATE INDEX IF NOT EXISTS warm_hint_node_id_idx
    ON warm_hint (node_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Reverse: drop the index and the table. The pg_notify channel name
-- is unregistered by removing every LISTEN at consumer shutdown; the
-- channel itself persists in Postgres until the cluster restarts
-- (acceptable; not a correctness issue).
DROP INDEX IF EXISTS warm_hint_node_id_idx;
DROP TABLE IF EXISTS warm_hint;
-- +goose StatementEnd
