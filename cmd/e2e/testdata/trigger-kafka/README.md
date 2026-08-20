# Issue #757 / ADR-118 — Kafka trigger poller e2e

## What this verifies

The fact that the kafka poller (pkg/sched/poller_kafka.go) makes the
broker hop correctly. This is the e2e counterpart to the unit tests
in pkg/sched/poller_kafka_test.go (commit 7 of the mega-PR):

  1. The kafka-go Reader connects to a real broker via the
     testcontainers/modules/kafka Run() helper
     (`confluentinc/confluent-local:7.5.0` by default).
  2. A producer written via kafka-go publishes a single message
     to the configured topic.
  3. The poller's Poll method FetchMessage's it: the
     SourceRecord payload, headers, and metadata all round-trip.
  4. The dispatcher's filter evaluates true (nil FilterCriteria
     in this test — the filter shape is pinned in
     pkg/sched/filter_test.go).
  5. The Ack path commits the consumer-group offset via
     kafka.NewReader.CommitMessages.
  6. A re-poll returns 0 records — the offset advance is durable.

## How to run

```bash
make e2e-kafka
# or
go test -tags kafka_e2e -count=1 -race ./pkg/sched/ -run TestKafkaPollerE2E
```

## Prerequisites

  - A running Docker daemon on the host. testcontainers-go spawns
    a `confluentinc/confluent-local` container and waits for the
    broker's :9092 port to be reachable.
  - The Go module `github.com/testcontainers/testcontainers-go/
    modules/kafka` must be present (already in go.sum at v0.40.0).
  - The `kafka_e2e` build tag is set so this test is excluded
    from the default `make test` surface.

## CI gate

The dispatch workflow's `needs: docker-check` precondition is
verified before the `e2e-kafka` job runs. If docker is unavailable
on the runner, the test is skipped (not failed) — this matches the
behaviour of the other testcontainer-based e2e tests in
`pkg/sched/`.

## Environment overrides

| Variable | Default | Purpose |
|----------|---------|---------|
| `KAFKA_TEST_IMAGE` | `confluentinc/confluent-local:7.5.0` | Pin a specific broker image (e.g. for reproducing a CI failure). |
| `SKIP_KAFKA_E2E` | (unset) | Set to any non-empty value to skip the test without bringing up Docker. |

## Cross-PR isolation

This testcontainer does NOT depend on the broker egress tc
shaper (commit 8) — the test uses a single 1-byte message
so the at-host cap is irrelevant. The kubernetes cgroup /
systemd v252+ IOBandwidthMax primitives are out of scope.

## Failure modes

| Symptom | Likely cause |
|---------|--------------|
| `dial: connection refused` | The container hasn't reached the broker-ready state yet; the test bumps this to 5s before bailing. |
| `poller poll error: kafka_poller: fetch: ...` | The broker config rejects the FetchMessage (auth, topic doesn't exist). See unit-test surface for the decode path. |
| `after Ack the second poll returned 1 records, want 0` | The consumer-group offset didn't commit. Check the `kafka.NewReader.CommitInterval` is `0` (explicit commits only) — pinned in commit 7. |
| `t.Skipf("testcontainers kafka not available ...")` | No Docker on host. Set `SKIP_KAFKA_E2E=1` to skip silently. |
