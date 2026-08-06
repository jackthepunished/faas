# ADR-075 · 90-day audit-event retention (ADR-035 close-out)

- **Status:** accepted
- **Date:** 2026-08-05
- **Issue:** IAM-4 / ADR-035 §"Out of scope" deferred work
- **Supersedes:** none

## Decision

Ship a daily audit-retention loop that prunes `events` rows older
than 90 days, closing the SOC 2 CC6.2 evidence-retention floor
gap that ADR-035 §"Out of scope" deferred.

The package is `pkg/eventretention` — a deliberate sibling of
`pkg/logintoken` and `pkg/grace` — exposing `Params`, `New`,
`Cleanup.Run(ctx)`, and `Cleanup.RunOnce(ctx)`. Wired into
`cmd/apid/main.go` next to the existing `loginTokenCleanup`
goroutine. The Store surface gains one new method:

```go
DeleteOldEvents(ctx context.Context, before time.Time) (int64, error)
```

Implementation is a single `DELETE FROM events WHERE at < $1` on
the Postgres side and a slice-filter on the in-memory twin so
tests can drive the loop without spinning up Postgres.

## Why

The audit pipeline (issue #286's failed-login emission, the
`auth.*` / `key.*` / `secret.*` / `account.*` / `stateless.*`
namespaces, and the future wake-timeline / sidecar surfaces)
keeps appending rows. At the observed rate the table grows
~3-4 GB/year per active-tier customer. Without a retention
trim, the table becomes a disk-fill liability long before any
real audit need ages out of usefulness.

ADR-035 deferred this with a one-line bullet in §"Out of scope";
that bullet is now removed.

## What ships

- `pkg/eventretention` (Run + RunOnce, mirrors `pkg/logintoken`)
- `state.Store.DeleteOldEvents` (one method, two implementations)
- `cmd/apid/main.go` wiring (one goroutine next to `loginTokenCleanup`)
- `pkg/eventretention/cleanup_test.go` (defaults, cutoff
  computation, error propagation, ctx-cancel, first-pass-error
  defence)
- `pkg/eventretention.DefaultCutoffDays = 90` (SOC 2 CC6.2 floor)

## What does NOT ship

- A backfill — there is no historical data to backfill; pre-PR
  rows simply age out under the new loop.
- A customer-facing config knob — 90 days is the audit policy
  floor, not a per-customer setting. Customer audit exports
  (issue #517, separate surface) read at the same cadence and
  don't need a separate retention ceiling.
- A migration — the `events` table already exists; the loop is a
  read-and-delete against the live rows. No DDL change.
- A new audit kind — retention is infrastructure, not a customer-
  visible event.

## Verification

`make test` (covers `pkg/eventretention`, `pkg/state/memstore`,
`pkg/state/pgstore` via the integration suite), `make lint`.
No metal re-run needed (no VM-lifecycle code touched). On the
the reference node the loop's first daily pass can be observed by setting the
interval to `1m` and watching `apid_audit_events_deleted_total`
(when the counter lands — out of scope for this PR, the count
is log-only for now).