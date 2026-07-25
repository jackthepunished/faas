-- filename: 00035_instances_state_check_realigns.sql
-- +goose Up
-- +goose StatementBegin

-- ADR-034: the SQL `instances_state_check` constraint has drifted
-- from `pkg/state/machine.go::States`. The Go declaration lists
-- eight values: parked, waking, cold_booting, running, snapshotting,
-- stopped, failed, evicting_account_deleting. The DB CHECK (last
-- touched by 00020) excludes 'snapshotting' and 'failed' — yet
-- schedd writes both:
--
--   * engine.go:1048 — UpdateInstanceStateWithTimestamp(...,
--     StateSnapshotting) on the Park path
--   * engine.go:438/554/603/784/815/825 — six StateFailed writes
--     (crash-loop, boot timeout, watchdog kill)
--
-- The drift is masked in CI because the engine tests use
-- MemStore (no CHECK), and no existing test seeds a 'snapshotting'
-- or 'failed' row via the public Store surface against real PG.
-- Migration 00028's partial index already lists 'snapshotting'
-- (line 75), and pgstore.go:1683/1824 filter on it — the CHECK is
-- the only piece that still disagrees.
--
-- Fix: realign the CHECK to `States ∪ {pending}` (pending is a
-- row-creation state with no Go constant but referenced by the
-- 00028 partial index). The fix uses the same NOT VALID → VALIDATE
-- race-free pattern as 00020: concurrent writes from apid (insert)
-- and schedd (transition) skip the constraint during the validate
-- pass; the validate pass itself scans the table once under
-- SHARE UPDATE EXCLUSIVE (reads/writes allowed). The hard
-- guarantee: at no point in this migration is `state` allowed to
-- take a value outside the new set *for new rows*.
--
-- Existing rows are 100% within the new set by construction:
-- parked / stopped / evicting_account_deleting rows all hold
-- values that are in the corrected set, and 'snapshotting' /
-- 'failed' are transient states (writes-then-leaves, no instance
-- idles in either state for the duration of a deploy). The
-- validate pass is a no-op on a clean dataset.

alter table instances
  drop constraint if exists instances_state_check;

alter table instances
  add constraint instances_state_check
    check (state in (
      'pending',
      'parked',
      'waking',
      'cold_booting',
      'running',
      'snapshotting',
      'stopped',
      'failed',
      'evicting_account_deleting'
    )) not valid;

alter table instances
  validate constraint instances_state_check;

-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin

-- The down-migrate reverses to the 00020-era CHECK set (without
-- 'snapshotting' or 'failed'). This is the inverse of the
-- up-migrate, NOT the current trunk: a clean reverse (apply 00035,
-- then down) leaves the DB on the 00020-era set, which is the
-- shape 00020's own down-migrate would have produced.
--
-- Consequence: a rollback of THIS migration on a database that
-- has rows in 'snapshotting' or 'failed' will fail. Spec §6.1
-- insists schedd writes those values transiently; a rollback
-- while instances are mid-transition would leave the constraint
-- out of sync with the wire. Operators executing the down-
-- migrate must guarantee the fleet is at rest first.

alter table instances
  drop constraint if exists instances_state_check;

alter table instances
  add constraint instances_state_check
    check (state in (
      'pending',
      'cold_booting',
      'waking',
      'running',
      'parked',
      'stopped',
      'evicting_account_deleting'
    ));
-- +goose StatementEnd
