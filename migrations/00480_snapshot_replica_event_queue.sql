-- +goose Up
-- +goose StatementBegin

-- Issue #1054 / ADR-063 scale follow-up.
--
-- The first snapshot fan-out implementation reconciled every active node by
-- scanning the complete snapshots table once per worker tick. That is safe,
-- but it makes the steady-state database cost O(active_nodes * snapshots)
-- every second. Keep the per-(snapshot,node) replica queue, but add one
-- append-only event per snapshot and a per-node cursor. Workers now consume
-- only snapshot events newer than their cursor; a node joining later simply
-- starts at cursor zero and catches up once.

CREATE TABLE IF NOT EXISTS snapshot_fanout_events (
    id          bigserial PRIMARY KEY,
    snapshot_id uuid NOT NULL UNIQUE REFERENCES snapshots(id) ON DELETE CASCADE,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS snapshot_fanout_events_snapshot_idx
    ON snapshot_fanout_events (snapshot_id);

CREATE TABLE IF NOT EXISTS snapshot_replica_cursors (
    node_id       uuid PRIMARY KEY REFERENCES compute_nodes(id) ON DELETE CASCADE,
    last_event_id bigint NOT NULL DEFAULT 0 CHECK (last_event_id >= 0),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- Seed the event stream for snapshots that existed before this migration.
-- Existing replica rows remain authoritative and ON CONFLICT keeps this
-- backfill harmless on a replay or a partially migrated database.
INSERT INTO snapshot_fanout_events (snapshot_id)
SELECT id
  FROM snapshots
 WHERE storage_key <> ''
ON CONFLICT (snapshot_id) DO NOTHING;

-- A snapshot row is immutable apart from stale/storage bookkeeping. Emit one
-- event for every newly published usable snapshot; workers apply the origin
-- and active-node filters when they consume it.
CREATE OR REPLACE FUNCTION snapshot_fanout_event_on_snapshot()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.storage_key <> '' AND NOT NEW.stale THEN
        INSERT INTO snapshot_fanout_events (snapshot_id)
        VALUES (NEW.id)
        ON CONFLICT (snapshot_id) DO NOTHING;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS snapshot_fanout_event_after_snapshot
    ON snapshots;
CREATE TRIGGER snapshot_fanout_event_after_snapshot
    AFTER INSERT OR UPDATE OF storage_key, stale ON snapshots
    FOR EACH ROW
    EXECUTE FUNCTION snapshot_fanout_event_on_snapshot();

-- imaged records origin metadata immediately after CreateSnapshot. The
-- snapshot event can therefore be consumed before the origin write commits.
-- Reconcile the affected snapshot here so a race cannot leave a cross-region
-- or producer-node replica queued.
CREATE OR REPLACE FUNCTION snapshot_replica_refresh_after_origin()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    DELETE FROM snapshot_replicas r
    USING compute_nodes cn
    WHERE r.snapshot_id = NEW.snapshot_id
      AND r.node_id = cn.id
      AND (
          (NEW.node_id IS NOT NULL AND r.node_id = NEW.node_id)
          OR (NEW.region <> '' AND coalesce(cn.region, '') <> NEW.region)
      );

    INSERT INTO snapshot_replicas (snapshot_id, node_id, region)
    SELECT NEW.snapshot_id, cn.id, coalesce(cn.region, '')
      FROM compute_nodes cn
      JOIN snapshots sn ON sn.id = NEW.snapshot_id
     WHERE cn.active
       AND sn.stale = false
       AND sn.storage_key <> ''
       AND (NEW.node_id IS NULL OR cn.id <> NEW.node_id)
       AND (NEW.region = '' OR coalesce(cn.region, '') = NEW.region)
    ON CONFLICT (snapshot_id, node_id) DO NOTHING;

    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS snapshot_replica_refresh_after_origin
    ON snapshot_origins;
CREATE TRIGGER snapshot_replica_refresh_after_origin
    AFTER INSERT OR UPDATE OF node_id, region ON snapshot_origins
    FOR EACH ROW
    EXECUTE FUNCTION snapshot_replica_refresh_after_origin();

-- A node that becomes active after the event has already been consumed must
-- still receive the existing snapshot set. The worker cursor handles normal
-- startup; this trigger closes the inactive->active and locality-change race
-- without putting a full scan back into the steady-state tick.
CREATE OR REPLACE FUNCTION snapshot_replica_refresh_after_compute_node()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.active THEN
        DELETE FROM snapshot_replicas r
        USING snapshots sn
        LEFT JOIN snapshot_origins so ON so.snapshot_id = sn.id
        WHERE r.snapshot_id = sn.id
          AND r.node_id = NEW.id
          AND (
              (so.node_id IS NOT NULL AND so.node_id = NEW.id)
              OR (so.region <> '' AND coalesce(NEW.region, '') <> so.region)
          );

        INSERT INTO snapshot_replicas (snapshot_id, node_id, region)
        SELECT e.snapshot_id, NEW.id, coalesce(NEW.region, '')
          FROM snapshot_fanout_events e
          JOIN snapshots sn ON sn.id = e.snapshot_id
          LEFT JOIN snapshot_origins so ON so.snapshot_id = sn.id
         WHERE sn.stale = false
           AND sn.storage_key <> ''
           AND (so.node_id IS NULL OR so.node_id <> NEW.id)
           AND (so.region = '' OR coalesce(NEW.region, '') = so.region)
        ON CONFLICT (snapshot_id, node_id) DO NOTHING;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS snapshot_replica_refresh_after_compute_node
    ON compute_nodes;
CREATE TRIGGER snapshot_replica_refresh_after_compute_node
    AFTER INSERT OR UPDATE OF active, region ON compute_nodes
    FOR EACH ROW
    EXECUTE FUNCTION snapshot_replica_refresh_after_compute_node();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS snapshot_replica_refresh_after_compute_node
    ON compute_nodes;
DROP FUNCTION IF EXISTS snapshot_replica_refresh_after_compute_node();
DROP TRIGGER IF EXISTS snapshot_replica_refresh_after_origin
    ON snapshot_origins;
DROP FUNCTION IF EXISTS snapshot_replica_refresh_after_origin();
DROP TRIGGER IF EXISTS snapshot_fanout_event_after_snapshot
    ON snapshots;
DROP FUNCTION IF EXISTS snapshot_fanout_event_on_snapshot();
DROP TABLE IF EXISTS snapshot_replica_cursors;
DROP TABLE IF EXISTS snapshot_fanout_events;
-- +goose StatementEnd
