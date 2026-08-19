# Issue #757 / ADR-100 — NATS trigger dispatch e2e

## What this verifies

The cross-component tripwire that the unified `Trigger` primitive
(issue #757 / ADR-100) is wired end-to-end for `kind=nats`:

  1. Trigger row inserted with `kind=nats` + valid `nats://...` config.
  2. NATS broker delivers a message on the configured subject.
  3. `pkg/sched/poller_nats.go::natsPoller` polls the durable
     consumer and mints a `trigger_records` row.
  4. `pkg/sched/dispatch_triggers.go::runTriggerTick` claims the row,
     posts the batch envelope to `pkg/gateway/synth.go`'s
     `handleInvocationDispatchBatch`.
  5. The function under the trigger returns
     `{"batchItemFailures":[]}` (full success) and the
     `trigger_records` row transitions to `state='succeeded'`.
  6. `audit_trigger.fired` row in the account's audit list.
  7. The broker Ack path (NATS JetStream Msg.Ack) is exercised.

## What it does NOT cover

  - `kind=kafka` / `kind=redis_streams` / `kind=sqs_compat` /
    `kind=queue` — these have their own per-broker unit tests
    in `pkg/sched/poller_*_test.go`.
  - Cross-cluster PG replication — this is a single-PG e2e.
  - VM lifecycle — `make test-metal` is the source of truth.

## Operating modes

### Mode A — NATS testcontainer (preferred, real-broker path)

Bring up a NATS server with `nats` and JetStream enabled:

```
docker run -d --name faas-nats-test -p 4222:4222 nats:2.10 \
    -js -sd /data -m 8222
```

Run the e2e via the `//go:build nats_e2e` tag (see
`trigger_nats_e2e_test.go`). Skip in default `make test` —
the test requires a TCP-broker on `:4222`. CI calls it via
`FAAS_NATS_URL=nats://localhost:4222` once the broker is
reachable.

### Mode B — in-process (default `make test`, CI-safe)

`poller_nats_test.go` in `pkg/sched/` exercises the same
adapter logic via an embedded test broker server. The
pkg-level unit test is the in-loop guard for the dispatch
shape; this e2e file is the cross-process tripwire that
asserts the full schedd ↔ gateway path against real Postgres.
