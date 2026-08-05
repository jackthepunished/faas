# ADR-075 · Per-app eviction priority (best_effort vs reserved)

- **Status:** accepted
- **Date:** 2026-08-05
- **Issue:** #475
- **Supersedes:** none

## Decision

Ship seven atomic commits that close the per-app reserved eviction
tier end-to-end: migration → limits + plan rows → state layer →
reaper tier-aware sort + counter → apid PATCH + RFC 7807 + audit →
SDK + OpenAPI sync → gregale flag + ADR + STATUS.

## Why

Today every app is treated identically under RAM pressure: schedd
parks LRU by `last_request_at` (spec §4.3). Customers with 24/7
healthcheck traffic have no way to tell the platform "don't park me
to make room for someone else's bursty workload." Issue #475
introduces a per-app `eviction_priority` column
('best_effort' | 'reserved'):

- `best_effort` keeps the existing LRU behaviour bit-for-bit.
- `reserved` still obeys idle / per-account / per-app caps but is
  protected from cross-account RAM-pressure eviction — every
  `best_effort` candidate is exhausted first.

This is deliberately not Lambda's two-tier model: pre-warm pools
violate ADR-005 (cold boot must always work), and per-tenant RAM
caps would blow the financial model. The `reserved` tier protects
against eviction; it does not keep instances resident.

## Decisions

### 3.1 Per-plan gate: `EvictionPriorityReservedAllowed bool`

Plan gating follows the same shape as `WarmSnapshotAllowed` and
`CronLimitPerAccount`: a closed boolean table in
`pkg/api/limits.go`. The Free plan unlocks no features that have a
real cost (ADR-027 floor economics); Free is the abuse-floor tier,
so `EvictionPriorityReservedAllowed = false` for Free. Hobby / Pro /
Scale unlock the tier.

### 3.2 Per-account cap: `ReservedConcurrencyPerAccount int` (apps, not instances)

The cap counts APPS, not instances. Hobby 1, Pro 2, Scale 4. The
issue body wording ("the 5th reserved app") is the binding
interpretation — a single reserved app with 5 concurrent instances
counts as 1 against the cap. Hobby's `MaxConcurrency` of 2 already
bounds the runaway; the reserved cap is a customer-facing *feature*
gate, not a memory gate.

The cap is enforced in apid's updateApp path (mirrors
`CreateCronIfUnderQuota`'s `accounts` row FOR UPDATE lock). A
racing PATCH from the same account could land one extra reserved
app — the cap is advisory, not strict. The financial model's
per-account RAM cap (47,600 MB) is the hard backstop so a cap race
costs nothing on the box.

### 3.3 Schema: closed enum + replay-safe migration

```sql
ALTER TABLE apps ADD COLUMN IF NOT EXISTS eviction_priority text
    NOT NULL DEFAULT 'best_effort';
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                   WHERE conname = 'apps_eviction_priority_chk'
                     AND conrelid = 'apps'::regclass) THEN
        ALTER TABLE apps ADD CONSTRAINT apps_eviction_priority_chk
            CHECK (eviction_priority IN ('best_effort', 'reserved'));
    END IF;
END$$;
```

The DO-block + `pg_catalog.pg_constraint` guard mirrors migration
00082 / 00109 / 00110. Postgres has no `ADD CONSTRAINT IF NOT EXISTS`,
so the guard is load-bearing for replay-safety on a re-run of the
migration.

No partial index on `apps(eviction_priority)` — the per-account
counter is bounded to ~4 rows per account in the worst case and
runs under an apps-row lock.

### 3.4 Reaper sort: tier-first, then plan, then LRU, then ID

`SelectEvictions` (`pkg/sched/reaper.go:333`) extends the sort
comparator with a tier early-out:

```
1. best_effort before reserved   ← NEW (issue #475)
2. non-Scale before Scale
3. LRU by LastRequest
4. Instance ID for determinism
```

`ReapIdle` and `ReapAggressive` are unchanged — the tier does not
grant immortality. The "idle-still-park" guarantee is enforced by
*not* touching those paths.

Pre-#475 rows have `EvictionPriority == ""`. The comparator
treats the empty string as `!reserved`, so the historical LRU
ordering is preserved bit-for-bit. The `resolvePriority` helper
in `pkg/sched/loop.go` applies the same empty→`best_effort`
fallback when stamping the per-tier counter.

### 3.5 Counter: `schedd_evicted_priority_total{priority, reason}`

A closed 6-tuple set: `{best_effort, reserved} × {idle,
eviction_aggressive, eviction_ram}`. Pre-instantiated at boot
(`pkg/wire/metrics.go`) so the §12 panel has zero rows from idle
fleet. The success criterion is `best_effort ≫ reserved` on
`{reason="eviction_ram"}` — a non-zero rate over a 5-minute window
on the reserved label is the alert.

The counter is incremented from `pkg/sched/loop.go` after every
successful park in the idle / aggressive / RAM-pressure branches.
Audit emission is from apid on PATCH only — the reaper's selection
effect is observable via the new counter, not via a per-eviction
audit row (would be far too noisy on a busy box).

### 3.6 Audit: `app.eviction_priority_changed` from apid on PATCH

A single-purpose, single-keyword-greppable audit row carries
`{old, new, app_id, slug}` when the value actually changes. A
no-op PATCH (same value) emits nothing. Subject is
`&app.AccountID` (matches `app.updated`'s shape at
`cmd/apid/handlers_ext.go:629`).

The `gregale audit-events --kind-prefix eviction_priority` filter
sees every tier change without parsing the larger `app.updated`
payload. The `cmd/gregale` filter is open (no closed-set whitelist
on `kind_prefix`), so the new kind is filterable without a code
change to the CLI.

### 3.7 CLI: single string flag (`--eviction-priority=best_effort|reserved`)

The CLI uses a single string flag rather than the warm-snapshot
boolean pair because the closed enum has only two values. A flip
down to `best_effort` is just `...=best_effort`. A flip up to
`reserved` triggers the server-side plan gate + per-account cap.
The CLI validates the value locally so a typo surfaces as a usage
error before the round-trip.

The `gregale app <slug>` text output surfaces the current tier so a
customer can verify their PATCH round-tripped without dropping
into `--json`.

### 3.8 No runtime feature flag

The migration defaults every existing app to `best_effort`, so the
sort change in `SelectEvictions` is a no-op for pre-#475 rows
(empty-string tier falls through to the existing
plan/LRU/ID comparator branches). A flag would add surface area
without buying safety — the migration is non-destructive.

### 3.9 SDK shape: thin one-liner + bundled field

`sdk/go/internal/api/client.go::SetAppEvictionPriority(ctx, slug,
priority)` is a thin one-liner PATCH-via-`Client.do`. The same
field is exposed on `UpdateAppRequest.EvictionPriority *string` so
callers that bundle the field into a wider PATCH don't have to
make a separate round-trip.

## Rejected alternatives

- **Lambda-style provisioned concurrency (pre-warm pool).** Rejected:
  violates ADR-005 (cold boot must always work) and per-tenant RAM
  caps would blow the financial model. The `reserved` tier protects
  against eviction; it does not keep instances resident.
- **Per-instance tier (not per-app).** Rejected: the issue body is
  explicitly per-app ("the 5th reserved app"). Per-instance tier
  would multiply the per-account cap by max_concurrency and break
  the Hobby=1 cap. Tier is a customer-facing *feature* gate.
- **Count instances (not apps) for the per-account cap.** Rejected:
  the issue body wording is "5th reserved app" — apps is the
  binding interpretation. Per-instance would let a Hobby customer
  with 1 app at MaxConcurrency=2 have 2 reserved instances, which
  exceeds the cap's intent.
- **Per-account row FOR UPDATE lock around the cap check.** Rejected:
  the cap is advisory; the financial model's per-account RAM cap
  is the hard backstop. The lock would block cross-account
  PATCHes for nothing.
- **Per-tier closed-set registry in `cmd/gregale`.** Rejected: the
  gregale `audit-events --kind-prefix` is an open filter — adding
  a new kind is a code change in the emitter, not in the CLI.
- **Implement the 5th gate (request-count).** Rejected: out of
  scope for #475; the per-account cap is the load-bearing gate.
- **Strict per-instance reserved count.** Rejected: same reason as
  the cap, plus the per-instance dimension is unbounded by
  MaxConcurrency (autoscale can run up to it).

## Critical reference files

| Concern | Path |
|---|---|
| Migration | `migrations/00135_apps_eviction_priority.sql` |
| Migration test | `migrations/00135_apps_eviction_priority_test.go` |
| Limits + plan rows | `pkg/api/limits.go` |
| Accessor tests | `pkg/api/limits_test.go` |
| App state | `pkg/state/types.go` |
| PgStore round-trip | `pkg/state/pgstore.go` (appsSelectColumns + scanAppInto) |
| MemStore mirror | `pkg/state/memstore.go` |
| Per-account cap read | `pkg/state/pgstore.go::CountAppsWithEvictionPriority` |
| Reaper sort | `pkg/sched/reaper.go::SelectEvictions` |
| Reaper counter | `pkg/wire/metrics.go::evictedPriority` |
| Loop carrier + counter | `pkg/sched/loop.go` |
| RFC 7807 codes | `pkg/api/errors.go` |
| DTO field | `pkg/api/dto.go::UpdateAppRequest` |
| PATCH handler | `cmd/apid/handlers_ext.go::updateApp` |
| Audit kind | `cmd/apid/handlers_ext.go` (single-purpose row) |
| OpenAPI | `api/openapi.yaml` + `pkg/apid/openapi.yaml` |
| SDK helper | `sdk/go/internal/api/client.go::SetAppEvictionPriority` |
| CLI flag | `cmd/gregale/commands2.go` (--eviction-priority) |
