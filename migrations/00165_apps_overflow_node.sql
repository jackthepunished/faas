-- +goose Up
-- +goose StatementBegin

-- filename: 00165_apps_overflow_node.sql
--
-- Tier A10 — per-app overflow_node preference (ADR-088).
--
-- After Tier A9 (ADR-087, migration shipped prior), an app whose
-- owner is sustained-at-capacity gets rebalanced to the first
-- peer-with-headroom (sorted by name ASC). That's fine for the
-- engine, but doesn't help a customer who already knows which
-- peer they want for a specific app — say, east-coast pusher
-- vs. west-coast intake. There's no surface today to declare
-- that preference.
--
-- apps.overflow_node is the customer's per-app preference. It's
-- a UUID NULL with a FOREIGN KEY to compute_nodes(id); ON DELETE
-- SET NULL (not RESTRICT) so draining a node doesn't strand apps
-- whose only preference was that node. The wire field is the
-- human-readable compute_nodes.name; apid resolves to the UUID
-- server-side via Store.ComputeNodeByName (cmd/apid/
-- compute_nodes.go:250).
--
-- The empty-uuid CHECK mirrors apps_node_id_nonempty_chk from
-- 00090 — Postgres' gen_random_uuid() default never produces
-- 00000000-0000-0000-0000-000000000000, so this is purely a
-- tripwire against a buggy INSERT path that produces
-- "uninitialised" rows.
--
-- Partial index apps_overflow_node_idx covers apps WHERE
-- overflow_node IS NOT NULL — the engine's hot path is "find
-- apps with an overflow_node set"; a NULL preference is the
-- "no preference" default and is the common case (most apps in
-- the fleet do not declare an explicit spill target). Indexing
-- only the non-NULL tail keeps the index narrow.
--
-- Replay-safety: every ADD COLUMN is IF NOT EXISTS, every
-- constraint add is paired with a DROP IF EXISTS in the down
-- block, every index add is paired with DROP INDEX IF EXISTS.
-- A second MigrateUp() is a no-op (PR #377 / ADR-041
-- contract).

alter table apps
  add column if not exists overflow_node uuid;

alter table apps
  drop constraint if exists apps_overflow_node_chk;

alter table apps
  add constraint apps_overflow_node_chk
    check (overflow_node is null or overflow_node <> '00000000-0000-0000-0000-000000000000');

alter table apps
  drop constraint if exists apps_overflow_node_fkey;

alter table apps
  add constraint apps_overflow_node_fkey
    foreign key (overflow_node) references compute_nodes(id)
      on delete set null;

create index if not exists apps_overflow_node_idx
  on apps (overflow_node)
  where overflow_node is not null;

-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin

-- Reverse order of creation. SET NULL semantics keep the
-- down path droppble without orphan rows.

drop index if exists apps_overflow_node_idx;

alter table apps
  drop constraint if exists apps_overflow_node_fkey;

alter table apps
  drop constraint if exists apps_overflow_node_chk;

alter table apps
  drop column if exists overflow_node;

-- +goose StatementEnd
