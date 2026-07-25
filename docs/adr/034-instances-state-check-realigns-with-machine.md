# ADR-034 · Realign `instances_state_check` with the Go state machine

- **Status:** accepted
- **Date:** 2026-07-25
- **Decision:** Drop and re-add `instances_state_check` so its allowed
  set equals `pkg/state/machine.go::States ∪ {pending}`. The DB
  reaccepts `snapshotting` and `failed` — the two values schedd
  already writes — closing the latent CHECK violation that is
  currently masked by `MemStore` in tests. Go is the source of truth
  for state values; the SQL CHECK is a defence-in-depth mirror.

## Context

`pkg/state/machine.go::States` (the single authoritative Go
declaration of the lifecycle) lists eight values:

```
parked, waking, cold_booting, running, snapshotting, stopped,
failed, evicting_account_deleting
```

The `transitions` map (machine.go:44) declares legal edges out of
`running` (→ `snapshotting`), `waking` (→ `failed`), and others.

schedd writes both `snapshotting` and `failed` to the DB every
day:

- `pkg/sched/engine.go:1048` — `UpdateInstanceStateWithTimestamp(
  ins.ID, string(state.StateSnapshotting), now)` on the Park path.
- `pkg/sched/engine.go:438/554/603/784/815/825` — six
  `StateFailed` writes (crash-loop, boot timeout, watchdog kill).

But the SQL CHECK that mirrors the Go set is **stale**:

```sql
-- migrations/00020_instance_evicting_state.sql (latest to touch
-- the CHECK)
check (state in (
  'pending','cold_booting','waking','running','parked','stopped',
  'evicting_account_deleting'
))
```

Both `snapshotting` and `failed` are absent. So a real Postgres
deployment rejects every `RUNNING→SNAPSHOTTING` and `RUNNING→FAILED`
write with `SQLSTATE 23514 check_violation`. Worse, the migration
is contradicted by a sibling file already on main:

- `migrations/00028_instances_wake_id.sql:75` — partial index
  `where state in ('waking','cold_booting','running','snapshotting','parked')`
  which accepts `snapshotting` as a value the column can hold.
- `pkg/state/pgstore.go:1683` — `ListAllInstances` reads
  `where state in ('running','waking','cold_booting','snapshotting')`.
- `pkg/state/pgstore.go:1824` — watchdog's
  `case when state = 'snapshotting' then parked_at else started_at`.
- `pkg/state/pgstore.go:2227` — comment explicitly classes
  `snapshotting` as parked-from-RAM.

The drift is **invisible in CI** because:

1. schedd's engine tests use `state.NewMemStore()` (62 of the
   engine's test surfaces). `MemStore.UpdateInstanceState` sets
   `ins.State = state` with no validation. The check would never
   fire even if a stricter Go-side guard existed.
2. No existing test inserts an `Instance` with `state =
   'snapshotting'` or `state = 'failed'` via the **public Store
   surface** against a real `PgStore`. The drift between migration
   00020 and the rest of the codebase has been hiding behind the
   test fake.

The gap is documented in code: `pkg/state/pgstore_test.go:1411-1413`
labels the watchdog's `snapshotting` CASE branch as
`COVERAGE GAP` — "the public Store surface cannot seed a row with
that state — this test exercises only the ELSE branch".

The first customer-visible impact today is the Park path:
`engine.go:1048` logs-and-continues on write failure, so the
`parked_at` anchor is silently lost on `RUNNING→SNAPSHOTTING` —
the row never parks, the snapshot never serializes, the instance
stays counted as live (§6.2-2 RAM accounting drifts upward). The
FAILED writes are not so forgiving: a watchdog `UPDATE` failure
would surface in meterd as a "permabooted" instance that never
auto-recovers.

The financial model is also at risk: the M7 acceptance gate
(§14) requires `instances` to converge from a `waking` row to a
`running` row to a `parked` row inside a single billing window —
the `RUNNING→SNAPSHOTTING` step is the load-bearing handoff.

## Decision

### Schema — append-only CHECK re-add

A new migration (slot 00035; verified at PR-creation time via
`make test` `TestMigrationsContiguous` /
`TestMigrationsUniquePrefixes`) drops the existing
`instances_state_check` and re-adds it with the corrected set:

```sql
check (state in (
  'pending','parked','waking','cold_booting','running',
  'snapshotting','stopped','failed','evicting_account_deleting'
))
```

The set is `States ∪ {pending}` — `pending` has no Go constant
(it is the Build-domain row in `instances` with `app_id != null`,
`state = 'pending'`, awaiting the first boot); it is referenced by
the partial index in 00028 and lives in the CHECK set the
Go state machine already includes implicitly. We add it on
purpose rather than expand the Go side, because the
`state_transitions` map (transitions:44) is the spec surface and
`pending` is a row-creation row, not a transition source.

Re-add uses the same NOT VALID → VALIDATE pattern as 00020:

1. `ALTER TABLE instances DROP CONSTRAINT IF EXISTS instances_state_check;`
2. `ALTER TABLE instances ADD CONSTRAINT instances_state_check
   CHECK (state in (...)) NOT VALID;`
3. `ALTER TABLE instances VALIDATE CONSTRAINT instances_state_check;`

Reasoning repeated from 00020: `NOT VALID` lets concurrent
INSERTs/UPDATEs from other daemons (apid inserts new instance
rows; schedd transitions existing ones) skip the constraint during
the validate pass. `VALIDATE CONSTRAINT` scans the table once
under `SHARE UPDATE EXCLUSIVE` (reads/writes allowed), not the
DROP+ADD pattern that leaves the column unconstrained for the
duration of the ADD. The hard guarantee: at no point in this
migration is `state` allowed to take a value outside the new set
*for new rows*.

Existing rows are 100 % within the new set by construction —
the `parked`/`stopped`/`evicting_account_deleting` rows all hold
values that are in the corrected set, and the missing values
(`snapshotting`, `failed`) are **transient states** that schedd
writes-then-leaves (no instance idles in `snapshotting` or
`failed` for the duration of a deploy). The validate pass is a
no-op on a clean dataset.

### Source-of-truth ordering — Go is authoritative

If a future value is added to `States` (e.g. M8 quota eviction
adds `quota_evicting`), the migration must follow within the
same PR. The new
`pkg/state/state_check_pgstore_test.go` test
(`TestPgStore_InstancesStateCheck_AcceptsAllMachineStates`) is the
tripwire that will fail on any future drift in either direction:

- It iterates `States` and persists one row per value via the
  public `CreateInstance` → `UpdateInstanceState` surface against
  a real `pgtest.Open(t)` PgStore. If the DB CHECK excludes any
  `States` value, the test fails with `SQLSTATE 23514`.
- It queries the live `instances_state_check` definition via
  `pg_constraint` and asserts the enum set extracted from the
  check equals `States ∪ {pending}`. If either side drifts
  (Go adds a value, or a future migration removes one), the test
  fails with a clear "drift" message naming the offending value.
- It inserts a row with `'bogus'` state and asserts the DB
  rejects with `SQLSTATE 23514`. The DB is not a no-op.

The pair of tests (write exhaustive + check-decomposition) pins
the contract from both ends so neither layer can silently diverge.

### Existing acceptance surface — unchanged

- `pkg/state/machine.go::States` — unchanged.
- `pkg/state/machine.go::transitions` — unchanged.
- `pkg/state/machine.go::CanTransition` — unchanged.
- `pkg/state/machine.go::IsLive` — unchanged.
- `pkg/state/machine.go::CountsForConcurrency` /
  `CountsForRAM` — unchanged.
- `pkg/sched/engine.go` — unchanged. The stores it calls
  (`UpdateInstanceStateWithTimestamp`, etc.) gain no new
  validation; the DB is the contract.
- `pkg/state/pgstore.go` — unchanged. The `snapshotting` and
  `failed` references in `ListAllInstances` (line 1683),
  `case when state = 'snapshotting'` (line 1824), and the
  doc comment at line 2227 are all now load-bearing on the
  new CHECK set, not floating.
- `migrations/00028_instances_wake_id.sql` — unchanged. Its
  partial index already includes `snapshotting`; the new CHECK
  brings the rest of the table into lockstep.

### New tests

- `pkg/state/state_check_pgstore_test.go` (new) — three tests,
  all against `pgtest.Open(t)`:
  1. `TestPgStore_InstancesStateCheck_AcceptsAllMachineStates` —
     iterate `States`, persist one row per value, assert success.
     This is the test that fails today and passes after the
     migration.
  2. `TestPgStore_InstancesStateCheck_SetMatchesMachineStates` —
     query `pg_constraint` for the live CHECK definition,
     extract the literal set, assert equality with
     `States ∪ {pending}`. The tripwire for future drift.
  3. `TestPgStore_InstancesStateCheck_RejectsBogusState` —
     insert `'bogus'`, assert `SQLSTATE 23514`.
- `pkg/state/machine_test.go` (extended) — one-line table
  test for `machine.go::IsLive` (the only `0 %` predicate in
  the file). Covers the four live states (WAKING, COLD_BOOTING,
  RUNNING, SNAPSHOTTING) and the four non-live states (PARKED,
  STOPPED, FAILED, EVICTING_ACCOUNT_DELETING). Strings are the
  documented `state.State` constants, not literals, so the
  test pins the semantic binding (e.g. `state.IsLive("failed")
  == false`).

### Down-migrate

The `-- +goose Down` direction restores the 00020-era set
**minus** `evicting_account_deleting` is not accepted — the
00020 down-migrate restores the pre-00020 set (without
`evicting_account_deleting`), so the new down-migrate reverses
to the 00020 set. This is the only correct shape: the down-
migrate is the inverse of the up-migrate, **not** the current
trunk. A clean reverse (apply 00035, then down) leaves the DB
on the 00020-era set, which matches 00020's down-migrate.

```
-- Down: restore the 00020-era CHECK set.
alter table instances
  drop constraint if exists instances_state_check;
alter table instances
  add constraint instances_state_check
    check (state in (
      'pending','cold_booting','waking','running','parked',
      'stopped','evicting_account_deleting'
    ));
```

## Consequences

- New migration `00035_instances_state_check_realigns.sql`.
  Re-verify slot at PR-creation time per the
  `migration-slot-renumber-at-pr-creation` memory.
- Test gate moves: `pkg/state` coverage on the CHECK predicates
  climbs from 0 % (`IsLive` is the only 0 % predicate) to ~80 %
  for the write/check/set-match surface.
- `pkg/state/pgstore_test.go:1411-1413` "COVERAGE GAP" comment
  can be removed in a follow-up commit (not this PR — it's a
  doc comment of a test that still exercises the ELSE branch;
  removing the comment requires a separate test for the THEN
  branch, which deploys with the rest of the new test file).
- The pre-00015 bug referenced in the test comment
  (`started_at` NULL silently mis-aged) is independent and
  does not regress.
- A future addition to `States` MUST be paired with a CHECK
  migration. The tripwire test makes this an enforced
  invariant, not a code-review accident.

## Rejected alternatives

- **Add `snapshotting` + `failed` only, leave `State` as
  list-of-strings:**
  equivalent in SQL effect, but couples the schema to the Go
  source-of-truth declaration. The drift accident is exactly
  what this ADR is preventing; future states would silently
  hit the same CHECK without it landing in `States`. Rejected.

- **Switch `State` to a `psql` enum and forget the CHECK:**
  adding a value to a Postgres enum takes a DDL change, but
  the `CHECK (state in (...))` form is symmetric with the Go
  side and reads the same way. The current `AppEgressAllowlist`
  precedent (ADR-031 + ADR-033) uses `cidr[]` + a trigger,
  not a custom enum, for the same reason: the surface is
  easier to compare across language boundaries when both
  sides are string literal sets. Rejected on symmetry.

- **Push the validation into the Go `Store` interface (i.e.
  check `State.Valid()` before every write):**
  would catch the illegal-write case in the Go-side test fake
  (`MemStore`) and the real DB, but breaks the existing
  `pgtest.Open`-style end-to-end behaviour. The DB is the
  real contract; the Go side is a defensive layer. Adding a
  Go-side `Valid()` check would mask future CHECK changes
  (the Go side would reject before the DB sees the row). The
  tripwire test catches the drift in both directions; a
  Go-side pre-check would only catch it in one. Rejected.

- **Wider scope: extend the tripwire to all `CHECK` constraints
  with this pattern (e.g. `apps.egress_allowlist_*`):**
  adjacent hygiene — `pkg/state` is the only Go ↔ SQL string
  enum today; the egress allowlist is a `cidr[]` with a
  trigger, not a string literal set. Out of scope for this
  PR; a follow-up ADR can generalize the tripwire pattern if
  a second string-enum table appears.

- **Skip the migration upgrade and instead fix the Go side
  to remove `snapshotting` / `failed`:**
  not viable. schedd writes these values because the spec
  requires them — `snapshotting` is the pause-before-snapshot
  step in the Park path (spec §6.1), `failed` is the
  crash-loop / boot-timeout outcome (spec §6.2 watchdog).
  Removing them from the Go side would break the spec.

## Cross-reference

- `migrations/00035_instances_state_check_realigns.sql` —
  append-only CHECK realign.
- `pkg/state/state_check_pgstore_test.go` (new) —
  three PgStore tripwire tests.
- `pkg/state/machine_test.go` (extended) — `IsLive` one-liner.
- `pkg/state/machine.go::States` — unchanged source of truth.
- `pkg/sched/engine.go:1048, 438, 554, 603, 784, 815, 825` —
  unchanged write sites (now load-bearing on the new CHECK).
- `pkg/state/pgstore.go:1683, 1824, 2227` — unchanged read
  sites (now within the new CHECK set).
- `migrations/00020_instance_evicting_state.sql` — predecessor
  to the wrong CHECK set.
- `migrations/00028_instances_wake_id.sql:75` — sibling partial
  index already accepting `snapshotting`, evidence the drift
  was known.
- `pkg/state/pgstore_test.go:1411-1413` — pre-existing
  "COVERAGE GAP" comment that this slice closes.
- ADR-026 — adds `evicting_account_deleting`; the Go ↔ SQL
  pattern this ADR mirrors.
- ADR-005 — cold-boot path that depends on `failed` being
  legal (the Park-from-failed edge).
- §6.1 — instance state machine.
- §6.2 — invariants the state machine enforces.
- §14 — M7 acceptance gate (depends on the
  `RUNNING→SNAPSHOTTING→PARKED` handoff).
