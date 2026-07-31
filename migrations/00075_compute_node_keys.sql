-- +goose Up
-- +goose StatementBegin

-- filename: 00075_compute_node_keys.sql
--
-- 00075_compute_node_keys.sql — ADR-053, Tier 1 Phase 2.
--
-- The compute_node_keys table holds one row per (compute_node,
-- signing-key-generation). vmmd registers its own row on startup
-- (mirroring the registerComputeNode UPSERT shape, idempotent on
-- (compute_node_id, key_id)). schedd reads the table into an
-- in-memory nodeKeyRegistry on startup and refreshes it on every
-- 'compute_node_changed' pg_notify (the same trigger migration
-- 00026 fires for the compute_nodes table).
--
-- key_id is the SHA-256 hex of the leaf's SubjectPublicKeyInfo
-- (the same value the wire's node_key_id carries). Two boxes that
-- happen to mint identical keys land in different compute_node_ids;
-- the PK is (compute_node_id, key_id), so a single key per node is
-- the typical shape. Key rotation (future ADR) adds rows under the
-- same compute_node_id with new key_ids; vmmd picks the freshest
-- row at startup.
--
-- public_key_pem is the ECDSA P-256 SubjectPublicKeyInfo (PEM-
-- encoded). The PEM shape mirrors pkg/cosign.LoadPublicKeyFile so
-- schedd's verify-side can parse it through the same loader (no
-- new decode path on the read side). Mode 0444 in spirit — the
-- column is non-secret by design.
--
-- Schema choices:
--
--   - PRIMARY KEY (compute_node_id, key_id) supports the typical
--     case (one row per node) AND future rotation (multiple rows
--     per node). The same shape as compute_node_changed's UPSERT
--     idempotency.
--
--   - ON DELETE CASCADE matches compute_nodes.id's lifecycle (an
--     operator hard-deleting a compute_nodes row cascades to the
--     signing key). The pre-slice-2 only-soft-delete posture
--     remains — a hard delete is gated behind the operator
--     DELETE /v1/compute-nodes endpoint and is rare.
--
--   - created_at default now() lets ops see when the key was
--     registered (rotation diagnostics).
--
-- Replay-safety (the contract migrations/replay_safety_test.go
-- asserts): every DDL is IF NOT EXISTS / DO-block-guarded. A
-- drifted box (schema present, goose row missing) re-applies
-- cleanly without tripping SQLSTATE 42P07 / 42710.

create table if not exists compute_node_keys (
    compute_node_id  uuid        not null references compute_nodes(id) on delete cascade,
    key_id           text        not null,
    public_key_pem   text        not null,
    created_at       timestamptz not null default now(),
    primary key (compute_node_id, key_id),
    -- key_id is the SHA-256 hex of the SubjectPublicKeyInfo (64
    -- lowercase hex chars). Pin the shape so a malformed key_id
    -- fails at INSERT instead of polluting the in-memory registry.
    constraint compute_node_keys_key_id_shape check
        (key_id ~ '^[a-f0-9]{64}$'),
    -- public_key_pem must be a parseable SubjectPublicKeyInfo PEM.
    -- Cheapest guard: must start with the BEGIN marker; deeper
    -- validation happens at schedd's verify-side parse.
    constraint compute_node_keys_pem_shape check
        (public_key_pem like '-----BEGIN PUBLIC KEY-----%')
);

-- The pg_notify channel 'compute_node_changed' (migration 00026)
-- already fires on every write to compute_nodes. We piggyback on
-- it for compute_node_keys — schedd's refresh is "any change to
-- compute_nodes OR compute_node_keys → re-read both". A dedicated
-- 'compute_node_keys_changed' trigger could narrow the wake set,
-- but it doubles the listener count for no operational benefit:
-- compute_nodes churns at most a few times per day (operator-
-- initiated), compute_node_keys churns at most once per box
-- restart. Bundling the refresh is fine.

create or replace function compute_node_keys_notify() returns trigger as $$
begin
    perform pg_notify('compute_node_changed', TG_TABLE_NAME);
    return null;
end;
$$ language plpgsql;

drop trigger if exists compute_node_keys_changed_trg on compute_node_keys;
create trigger compute_node_keys_changed_trg
    after insert or update or delete on compute_node_keys
    for each statement execute function compute_node_keys_notify();

-- Index by compute_node_id so the schedd-side startup scan and
-- the per-node pg_notify refresh are O(rows-for-this-node) instead
-- of full-table. Matches the existing compute_nodes_active_idx
-- pattern.
create index if not exists compute_node_keys_node_idx
    on compute_node_keys (compute_node_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

drop trigger if exists compute_node_keys_changed_trg on compute_node_keys;
drop function if exists compute_node_keys_notify();
drop index if exists compute_node_keys_node_idx;
drop table if exists compute_node_keys;

-- +goose StatementEnd
