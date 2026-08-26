-- +goose Up
-- +goose StatementBegin

-- Issue #1054 / ADR-063 follow-on: durable snapshot prepositioning.
--
-- Snapshot blobs are authoritative in the shared storage backend (OCI in a
-- multi-box deployment). This table is a resumable, node-local cache index:
-- one row says whether a compute node has pulled both restore blobs for one
-- snapshot. The worker may be interrupted between any two transitions and
-- will reclaim stale leases or retry failed rows on the next tick.
--
-- The table deliberately has no source-node column. A snapshot can be
-- produced by a node that is later drained, and every active node in the
-- fleet is a valid cache destination. Reconciliation on each node also means
-- a newly joined box catches up with existing snapshots without a special
-- deployment-time fan-out RPC.

CREATE TABLE IF NOT EXISTS snapshot_replicas (
    snapshot_id       uuid        NOT NULL REFERENCES snapshots(id) ON DELETE CASCADE,
    node_id           uuid        NOT NULL REFERENCES compute_nodes(id) ON DELETE CASCADE,
    region            text        NOT NULL DEFAULT '',
    state             text        NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'syncing', 'ready', 'failed')),
    attempts          integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error        text,
    next_attempt_at   timestamptz,
    ready_at          timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (snapshot_id, node_id)
);

CREATE INDEX IF NOT EXISTS snapshot_replicas_node_state_idx
    ON snapshot_replicas (node_id, state, next_attempt_at, created_at);

CREATE INDEX IF NOT EXISTS snapshot_replicas_snapshot_state_idx
    ON snapshot_replicas (snapshot_id, state, node_id);

-- Keep locality metadata separate so the existing snapshots scan shape stays
-- stable and legacy rows remain eligible for a safe catch-up fan-out.
CREATE TABLE IF NOT EXISTS snapshot_origins (
    snapshot_id uuid NOT NULL PRIMARY KEY REFERENCES snapshots(id) ON DELETE CASCADE,
    node_id     uuid REFERENCES compute_nodes(id) ON DELETE SET NULL,
    region      text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS snapshot_origins_region_idx
    ON snapshot_origins (region, snapshot_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS snapshot_origins;
DROP TABLE IF EXISTS snapshot_replicas;
-- +goose StatementEnd
