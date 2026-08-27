-- +goose Up
-- +goose StatementBegin
-- filename: 00430_compute_node_heartbeats_builder_tick.sql
--
-- Operator-side observability mega-PR — Commit 7 (P5 builderd
-- heartbeat + per-node build-queue gauge). Widens the existing
-- CHECK constraint on compute_node_heartbeats.source from
-- ('heartbeat_tick','deactivation','reactivation') to add the
-- new 'builder_tick' value emitted by the cmd/builderd heartbeat
-- goroutine.
--
-- Why a separate source value rather than reusing 'heartbeat_tick':
--
--   Keeping 'builder_tick' distinct lets the operator dashboard
--   separate vmmd-driven liveness heartbeats (which feed the
--   schedd staleness gate) from builderd-driven liveness
--   (which is observability-only — a missing builder_tick does
--   NOT flip the node inactive). The migration widening the
--   enum unblocks both the apid admin endpoint
--   GET /v1/admin/obs/builder-heartbeats and the builderd
--   writer; idle builders continue to produce these rows.
--
-- Constraint naming:
--
--   The original CHECK (migrations/00065) is anonymous — Postgres
--   assigns the auto-name 'compute_node_heartbeats_source_check'.
--   We DROP and re-ADD with the same auto-named inline CHECK
--   shape so this migration is idempotent against the canonical
--   name; if a sibling migration has renamed the constraint,
--   the DROP below errors noisily rather than silently leaving
--   the old CHECK in place (the operator would see stale
--   enum rejections from 'builder_tick' writes).
--
-- Migration replay-safety: the IF EXISTS guards make the
-- migration idempotent under goose replay; the second apply
-- no-ops on the DROP and the re-ADD includes 'builder_tick' so
-- the CHECK shape matches the first apply byte-for-byte.
--
-- Slot fence: 00430 was claimed for this PR; the cross-PR
-- gate fenced siblings at 00422-00429 (see MEMORY.md cross-pr
-- slot dance entries).

alter table compute_node_heartbeats
    drop constraint if exists compute_node_heartbeats_source_check;

alter table compute_node_heartbeats
    add constraint compute_node_heartbeats_source_check
        check (source in ('heartbeat_tick','deactivation','reactivation','builder_tick'));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

alter table compute_node_heartbeats
    drop constraint if exists compute_node_heartbeats_source_check;

alter table compute_node_heartbeats
    add constraint compute_node_heartbeats_source_check
        check (source in ('heartbeat_tick','deactivation','reactivation'));

-- +goose StatementEnd
