-- +goose Up
-- +goose StatementBegin
-- filename: 00065_compute_node_heartbeats.sql
--
-- CP-1: operator-observability heartbeat history for the compute_node
-- fleet. The existing compute_nodes row carries only the latest
-- last_heartbeat_at; that gives schedd a single-stamp gate but no
-- timeline for "what did the box look like over the last 30 min?".
-- This migration introduces an append-only history table that the
-- new operator endpoint GET /v1/compute-nodes/{name}/heartbeats
-- reads from.
--
-- Schema rationale:
--
--   id         bigserial — append-only, monotonic, useful for OFFSET
--              pagination if a future endpoint ever wants before_id.
--   node_id    uuid FK on delete cascade — when an admin issues a
--              hard DELETE FROM compute_nodes (DELETE /v1/compute-nodes/{name}?hard=1),
--              the history must drop with the row. Soft-delete
--              (SetComputeNodeActive(false)) preserves history because
--              it does NOT fire the FK.
--   received_at  timestamptz — wall-clock the heartbeat was received
--              (= now() in schedd's stamp path). The wire endpoint
--              uses this as the row's primary sort key.
--   last_heartbeat_at timestamptz — mirror of compute_nodes.last_heartbeat_at
--              at stamp time. The two timings SHOULD be near-equal
--              on a healthy node; a divergence is itself operator
--              signal (clock skew, schedd-watcher pause).
--   source     text — 'heartbeat_tick' on the routine stamp path,
--              'deactivation' when MarkComputeNodeInactive fires (the
--              watchdog's last contact attempt before flipping
--              active=false), 'reactivation' on the recovery path.
--              The CHECK constraint keeps the enum Go-shaped.
--
--   unique(node_id, received_at) is duplicate-prevention: the same
--   Postgres now() from a hot tick can collide on received_at. We
--   deliberately do NOT use ON CONFLICT DO NOTHING on the writer
--   side so a duplicate stamp is observable as a logged warning
--   rather than silent — a silently-deduped stamp would mask a
--   future bug where the scheduler tick fires twice.
--
--   index (node_id, received_at DESC) is the dedicated read path.
--   The endpoint filters by node_id and orders by received_at DESC
--   with a LIMIT; this composite is the matching shape.
--
-- Migration discipline: this migration is replay-safe (CREATE TABLE
-- IF NOT EXISTS, CREATE INDEX IF NOT EXISTS). No backfill is needed —
-- the table starts empty and the endpoint returns 0 rows for nodes
-- that pre-date the deployment.
--
-- Slot history: 65 (next free after 00064_invocations_dead_letter.sql).
-- Cross-PR slot gate applies; if a sibling open PR claims 65 at PR-open
-- time, CP-1 reserves via 00066_reserve_slot.sql and lands the actual
-- DML at 67 (mirrors the 00060_reserve_slot.sql + 00064 slot pattern).

create table if not exists compute_node_heartbeats (
    id                bigserial primary key,
    node_id           uuid not null references compute_nodes(id) on delete cascade,
    received_at       timestamptz not null default now(),
    last_heartbeat_at timestamptz not null,
    source            text not null check (source in ('heartbeat_tick','deactivation','reactivation')),
    constraint compute_node_heartbeats_node_at_uniq unique (node_id, received_at)
);

create index if not exists compute_node_heartbeats_node_at_idx
    on compute_node_heartbeats (node_id, received_at desc);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

drop index if exists compute_node_heartbeats_node_at_idx;
drop table if exists compute_node_heartbeats;

-- +goose StatementEnd
