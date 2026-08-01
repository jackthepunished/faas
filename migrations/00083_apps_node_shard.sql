-- +goose Up
-- +goose StatementBegin

-- filename: 00083_apps_node_shard.sql
--
-- Phase 2 / Gate A — durable app owner per compute node.
--
-- Pre-00083 `apps` has no node_id column: schedd chose the
-- placement at wake time, and one schedd was implicitly the
-- owner of every app because there was only one schedd. Going
-- to N peer-equal schedds (one per non-default-local active
-- node) requires a durable shard key so apid can route
-- customer-intent writes to the owner and so every schedd can
-- answer "is this app mine?" without a fleet-wide scan.
--
-- Two columns in this migration:
--
--   apps.node_id (FK to compute_nodes.id, NOT NULL after
--   backfill, indexed, empty-uuid CHECK, immutable by
--   convention — set once at CreateApp time, never PATCHed).
--   Existing rows are backfilled to the synthetic
--   default-local row seeded by migration 00024, so a
--   pre-Phase-2 deploy upgrades without a manual data
--   migration.
--
--   compute_nodes.schedd_target_url (text, NULLABLE,
--   scheme CHECK). Distinct from the existing
--   compute_nodes.target_url which is the vmmd target
--   (Firecracker + jailer). schedd_target_url is the
--   schedd gRPC target gatewayd dials when routing a
--   customer request to the app's owner schedd. Defaulted
--   on the synthetic default-local row to the canonical
--   unix socket so the single-box posture is preserved
--   bit-for-bit.
--
-- The owner-routing contract this migration enables:
--
--   * apid's createApp handler calls PlacementScheduler.Choose
--     inside the same transaction that calls
--     CreateAppIfUnderQuota, then writes the chosen node_id
--     on the apps row.
--   * schedd resolves its OwnerNodeID at startup from
--     cfg.NodeName → compute_nodes.id. Every gRPC handler
--     (Wake / AdmitInstance / ParkInstance / StreamAppLogs /
--     ReportActivity) calls authorizeApp/authorizeInstance
--     and returns codes.FailedPrecondition on a mismatch.
--   * The app_changed / deployment_changed / snapshot_prime
--     pg_notify broadcasts stay broadcast; each schedd filters
--     by app.node_id == OwnerNodeID before any engine call.
--     This closes the N-schedd duplicate-Prime hazard (today
--     every schedd would call Engine.Prime on every prime
--     notification — on a 5-node fleet that's 5× the
--     snapshot work for no benefit).
--   * gatewayd dials the per-node schedd client lazily from
--     a compute_node_changed-driven cache keyed by node_id
--     and backed by schedd_target_url. Customer traffic to
--     app X is routed to the schedd that owns X.
--
-- ADR-058's Tier A1 + Tier A2 work is the placement-side
-- prerequisite: the chooser is already per-node (usedVCPU
-- per node, ledger-floored UsedMB per node, the vCPU
-- admission gate is per-node). This migration is the
-- persistence-side prerequisite that turns the per-node
-- chooser into a per-node owner. Per ADR-025 axis 5,
-- sticky-warm placement is bias-not-gate, and the warm hint
-- does not enter the wire; the shard key is the only
-- owner-routing source of truth.
--
-- The CHECK constraints are load-bearing on three failure
-- modes: empty-uuid (rejects a 00000000-... default that
-- would silently bind every new app to a "no node" row),
-- nonzero FK target (rejects a typo'd compute_node_id that
-- would otherwise land as a 23503 foreign_key_violation on
-- the Insert rather than a clean reject at the apid
-- handler), and the schedd_target_url scheme check
-- (rejects an operator POST that sets "https://..." or
-- "/path/to/sock" — the wire.ParseTarget consumer at
-- gatewayd would crash on a non-(unix|tcp) scheme).
--
-- Replay-safety: every ADD COLUMN is IF NOT EXISTS, every
-- constraint add is paired with a DROP IF EXISTS in the
-- down block, and the column defaults / backfill UPDATE
-- are unconditional. A second MigrateUp() is a no-op; this
-- is the PR #377 / ADR-041 contract.

-- Step 1: add the apps.node_id column nullable so the
-- backfill UPDATE can run without a temporary default.

alter table apps
  add column if not exists node_id uuid;

-- Step 2: backfill every existing app to the synthetic
-- default-local row seeded by migration 00024. Single-box
-- installs see identical behaviour; multi-box installs
-- will need to rebalance (deliberately deferred — see
-- ADR-055 / Phase 2 follow-up section).

update apps
   set node_id = (select id from compute_nodes where name = 'default-local')
 where node_id is null;

-- Step 3: enforce the NOT NULL + the empty-uuid CHECK +
-- the FK to compute_nodes. The FK uses ON DELETE RESTRICT
-- because dropping a compute_node that still owns apps
-- would orphan customer state silently. (An operator who
-- wants to drop a node must first mark it inactive and
-- drain its apps — the gate-a.md runbook prescribes the
-- order.)

alter table apps
  alter column node_id set not null;

alter table apps
  drop constraint if exists apps_node_id_nonempty_chk;

alter table apps
  add constraint apps_node_id_nonempty_chk
    check (node_id <> '00000000-0000-0000-0000-000000000000');

alter table apps
  drop constraint if exists apps_node_id_fkey;

alter table apps
  add constraint apps_node_id_fkey
    foreign key (node_id) references compute_nodes(id) on delete restrict;

create index if not exists apps_node_id_idx on apps (node_id);

-- Step 4: add compute_nodes.schedd_target_url. The CHECK
-- is on the scheme only; the rest of the URL is opaque to
-- Postgres and validated at the consumer (gatewayd's
-- wire.ParseTarget refuses anything not matching
-- ^(unix|tcp):// anyway, so the DB-level check is a
-- tripwire for an operator POST that would otherwise
-- propagate to the dial layer and panic there).

alter table compute_nodes
  add column if not exists schedd_target_url text;

alter table compute_nodes
  drop constraint if exists compute_nodes_schedd_target_url_scheme_chk;

alter table compute_nodes
  add constraint compute_nodes_schedd_target_url_scheme_chk
    check (schedd_target_url is null or schedd_target_url ~ '^(unix|tcp)://');

-- Step 5: default the synthetic default-local row's
-- schedd_target_url to the canonical unix socket. Single-
-- box installs preserve bit-for-bit behaviour: the schedd
-- daemon is still dialed over /run/faas/schedd.sock. New
-- compute_nodes rows are intentionally NOT defaulted
-- (NULL is a valid state — schedd startup treats nil as
-- "not yet configured" and refreshes via the
-- compute_node_changed pg_notify cache; the operator must
-- set schedd_target_url explicitly via POST /v1/compute-nodes
-- during the add-a-node runbook).

update compute_nodes
   set schedd_target_url = 'unix:///run/faas/schedd.sock'
 where name = 'default-local'
   and schedd_target_url is null;

-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin

-- Drop in the reverse order of creation. The IF EXISTS
-- guards on the DROP CONSTRAINT calls are belt-and-braces
-- against a half-applied down path; the column DROPs
-- (without IF EXISTS) are the load-bearing ones.

drop index if exists apps_node_id_idx;

alter table apps
  drop constraint if exists apps_node_id_fkey;

alter table apps
  drop constraint if exists apps_node_id_nonempty_chk;

alter table apps
  drop column if exists node_id;

alter table compute_nodes
  drop constraint if exists compute_nodes_schedd_target_url_scheme_chk;

alter table compute_nodes
  drop column if exists schedd_target_url;

-- +goose StatementEnd
