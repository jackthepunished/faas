// poller_kafka_test.go — Kafka poller unit tests.
//
// The kafka poller's broker side (segmentio/kafka-go Reader) is
// hard to drive in-process — it owns a real network socket, a
// consumer-group coordinator, and a goroutine pump. We can't
// construct a Reader against a fake broker without an external
// test container. So instead of mocking the concrete Reader,
// this file drives the poison-record branch (Nack with
// reason="poison_record") through a stub that satisfies the
// kafkaBrokerOp interface introduced for this test.
//
// What the test pins (audit #10, PR #910 review):
//
//  1. With broker_poison_strategy="commit" (the default), the
//     kafka poller calls CommitMessages on poison. This preserves
//     the pre-migration behaviour byte-for-byte; existing
//     operators see no change.
//
//  2. With broker_poison_strategy="seek-to-offset", the kafka
//     poller calls SetOffset(msg.Offset) on poison instead. The
//     broker offset rewinds to the failed offset so the next
//     Poll re-fetches the same message — the operator-retry
//     path described in pkg/api/trigger.go::BrokerPoisonStrategySeekToOffset.
//
//  3. With broker_poison_strategy="" (the zero value), the
//     poller falls through to "commit" — same as #1. This is
//     the safety net for older trigger rows that pre-date
//     migration 00275 (the column has a DB DEFAULT 'commit', so
//     the Go side never sees "" in practice, but the test pins
//     the in-process contract too).
//
//  4. Non-poison Nack reasons always rewind via SetOffset
//     regardless of broker_poison_strategy — the strategy only
//     governs the poison-specific terminal behaviour.

package sched

import (
	"context"
	"sync"
	"testing"

	"github.com/segmentio/kafka-go"

	"github.com/onebox-faas/faas/pkg/state/sqlc"
)

// poisonStrategyOp records the terminal broker op the poller
// chose. The poller doesn't reach for CommitMessages or
// SetOffset directly on a non-poison Nack — those branches are
// not exercised by this test (the test pins audit #10, not the
// existing Nack-on-failure path).
type poisonStrategyOp int

const (
	poisonOpCommit poisonStrategyOp = iota
	poisonOpSetOffset
)

type poisonStrategyReader struct {
	mu        sync.Mutex
	poisonOp  poisonStrategyOp // last terminal op recorded on poison
	offsets   []int64
	commitCtx context.Context
}

// CommitMessages records the messages + the commit op. Mirrors
// the kafka.Reader.CommitMessages(ctx, msgs...) signature.
func (r *poisonStrategyReader) CommitMessages(ctx context.Context, msgs ...kafka.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commitCtx = ctx
	r.poisonOp = poisonOpCommit
	for _, m := range msgs {
		r.offsets = append(r.offsets, m.Offset)
	}
	return nil
}

// SetOffset records the rewind op + the offset.
func (r *poisonStrategyReader) SetOffset(offset int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.poisonOp = poisonOpSetOffset
	r.offsets = append(r.offsets, offset)
	return nil
}

// FetchMessage + Close are part of the kafkaBrokerOp interface
// but are not exercised by these tests (the tests drive Nack
// only). The stubs return zero values — kept here so the
// poisonStrategyReader satisfies the interface and the poller's
// construction compiles.
func (r *poisonStrategyReader) FetchMessage(ctx context.Context) (kafka.Message, error) {
	return kafka.Message{}, nil
}

func (r *poisonStrategyReader) Close() error {
	return nil
}

// kafkaNackFixture primes a kafkaPoller with one in-flight
// message + a stub broker. Returns the poller, the stub (for
// assertion), and the in-flight id.
func kafkaNackFixture(rdr *poisonStrategyReader, offset int64) (*kafkaPoller, string) {
	id := "0-42-100"
	return &kafkaPoller{
		reader:   rdr,
		batchMax: 100,
		inFlight: map[string]kafka.Message{
			id: {Topic: "orders.v1", Partition: 0, Offset: offset},
		},
	}, id
}

// TestKafka_NackPoisonCommit asserts that with
// broker_poison_strategy="commit" (the default), the poller
// commits the message — the previous behaviour, preserved
// byte-for-byte by the migration.
func TestKafka_NackPoisonCommit(t *testing.T) {
	t.Parallel()

	rdr := &poisonStrategyReader{}
	k, id := kafkaNackFixture(rdr, 42)
	trig := sqlc.Trigger{BrokerPoisonStrategy: "commit"}

	if err := k.Nack(context.Background(), trig, []string{id}, triggerReasonPoisonRecord); err != nil {
		t.Fatalf("Nack: %v", err)
	}
	rdr.mu.Lock()
	defer rdr.mu.Unlock()
	if rdr.poisonOp != poisonOpCommit {
		t.Fatalf("poison op = %v, want %v (commit strategy must CommitMessages)", rdr.poisonOp, poisonOpCommit)
	}
	if len(rdr.offsets) != 1 || rdr.offsets[0] != 42 {
		t.Fatalf("committed offsets = %v, want [42]", rdr.offsets)
	}
	if _, ok := k.inFlight[id]; ok {
		t.Fatal("Nack should drop the inFlight handle after CommitMessages")
	}
}

// TestKafka_NackPoisonSeekToOffset asserts that with
// broker_poison_strategy="seek-to-offset", the poller rewinds
// via SetOffset — broker offset returns to msg.Offset so the
// next Poll re-fetches the same message.
func TestKafka_NackPoisonSeekToOffset(t *testing.T) {
	t.Parallel()

	rdr := &poisonStrategyReader{}
	k, _ := kafkaNackFixture(rdr, 77)
	// Override the inFlight id for clarity (different offset).
	k.inFlight = map[string]kafka.Message{
		"0-77-200": {Topic: "orders.v1", Partition: 0, Offset: 77},
	}
	id := "0-77-200"
	trig := sqlc.Trigger{BrokerPoisonStrategy: "seek-to-offset"}

	if err := k.Nack(context.Background(), trig, []string{id}, triggerReasonPoisonRecord); err != nil {
		t.Fatalf("Nack: %v", err)
	}
	rdr.mu.Lock()
	defer rdr.mu.Unlock()
	if rdr.poisonOp != poisonOpSetOffset {
		t.Fatalf("poison op = %v, want %v (seek-to-offset strategy must SetOffset)", rdr.poisonOp, poisonOpSetOffset)
	}
	if len(rdr.offsets) != 1 || rdr.offsets[0] != 77 {
		t.Fatalf("rewound offsets = %v, want [77]", rdr.offsets)
	}
	if _, ok := k.inFlight[id]; ok {
		t.Fatal("Nack should drop the inFlight handle after SetOffset")
	}
}

// TestKafka_NackPoisonEmptyStrategyDefaultsToCommit pins the
// safety net: broker_poison_strategy="" (the zero value) falls
// through to "commit" so a trigger row that somehow ends up with
// an empty strategy — e.g. a pre-migration row that the migration
// applied to AFTER a default was added — still gets the previous
// behaviour. The DB DEFAULT 'commit' makes this unreachable in
// practice, but the in-process contract is pinned here too.
func TestKafka_NackPoisonEmptyStrategyDefaultsToCommit(t *testing.T) {
	t.Parallel()

	rdr := &poisonStrategyReader{}
	k, _ := kafkaNackFixture(rdr, 99)
	k.inFlight = map[string]kafka.Message{
		"0-99-300": {Topic: "orders.v1", Partition: 0, Offset: 99},
	}
	id := "0-99-300"
	trig := sqlc.Trigger{BrokerPoisonStrategy: ""}

	if err := k.Nack(context.Background(), trig, []string{id}, triggerReasonPoisonRecord); err != nil {
		t.Fatalf("Nack: %v", err)
	}
	rdr.mu.Lock()
	defer rdr.mu.Unlock()
	if rdr.poisonOp != poisonOpCommit {
		t.Fatalf("poison op = %v, want %v (empty strategy must default to commit)", rdr.poisonOp, poisonOpCommit)
	}
}

// TestKafka_NackBrokerErrorAlwaysRewinds asserts that non-poison
// Nack reasons (broker_error) always rewind via SetOffset,
// regardless of broker_poison_strategy. The strategy only
// governs the poison-specific terminal behaviour; transient
// broker failures must always re-poll.
func TestKafka_NackBrokerErrorAlwaysRewinds(t *testing.T) {
	t.Parallel()

	for _, stratName := range []string{"commit", "seek-to-offset", ""} {
		stratName := stratName
		t.Run("strat="+stratName, func(t *testing.T) {
			t.Parallel()

			rdr := &poisonStrategyReader{}
			k, id := kafkaNackFixture(rdr, 123)
			trig := sqlc.Trigger{BrokerPoisonStrategy: stratName}

			if err := k.Nack(context.Background(), trig, []string{id}, triggerReasonBrokerError); err != nil {
				t.Fatalf("Nack strat=%q: %v", stratName, err)
			}
			rdr.mu.Lock()
			defer rdr.mu.Unlock()
			if rdr.poisonOp != poisonOpSetOffset {
				t.Fatalf("broker_error strat=%q: poison op = %v, want %v (broker_error must always SetOffset)", stratName, rdr.poisonOp, poisonOpSetOffset)
			}
			if len(rdr.offsets) != 1 || rdr.offsets[0] != 123 {
				t.Fatalf("broker_error strat=%q: rewound offsets = %v, want [123]", stratName, rdr.offsets)
			}
		})
	}
}
