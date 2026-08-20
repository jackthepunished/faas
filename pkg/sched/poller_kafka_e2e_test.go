//go:build kafka_e2e

// poller_kafka_e2e_test.go — real-broker end-to-end test for
// the kafka poller path (issue #757 / ADR-118, commit 10 of
// the mega-PR).
//
// Build tag: kafka_e2e. Not part of the default `make test`
// surface — run via `go test -tags kafka_e2e -count=1 -race
// ./pkg/sched/ -run TestKafkaPollerE2E` or `make e2e-kafka`
// (added in this commit). Requires a running Docker daemon on
// the host; CI gates the workflow precondition via
// `needs: docker-check` (added in this commit to
// .github/workflows/ci.yml).
//
// What this test pins:
//
//  1. The kafka-go poller (pkg/sched/poller_kafka.go) connects
//     to a real Kafka broker via the testcontainers / kafka
//     module's Run() helper, with PLAINTEXT (no TLS, no SASL)
//     — the ~most-common customer shape.
//
//  2. A producer written via kafka-go publishes a message;
//     the poller's Poll method FetchMessage's it; the
//     dispatcher's filter evaluates true (nil FilterCriteria)
//     passes everything; the record is AcKed; the
//     consumer-group offset advances so a fresh Poll returns
//     0 messages.
//
//  3. The wire surface matches what cmd/apid would write:
//     the trigger row carries a JSON-encoded kafka config
//     matching the KafkaConfig schema in pkg/gregalemanifest;
//     the in-process decoder in decodeKafkaConfig handles
//     it without error.
//
// This is the e2e counterpart to the unit tests in
// poller_kafka_test.go (commit 7). The unit tests cover the
// decode + Dialer surface + the poison-record FSM; this test
// covers the actual network round-trip — the broker hop
// that the kafka-go Reader makes when FetchMessage is called
// and the consumer-group offset commit when CommitMessages
// is called.

package sched

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	kafkago "github.com/segmentio/kafka-go"
	tckafka "github.com/testcontainers/testcontainers-go/modules/kafka"

	"github.com/onebox-faas/faas/pkg/state/sqlc"
)

// TestKafkaPollerE2E_HappyPath brings up a single-node Kafka
// container, configures the kafka poller against it, publishes
// a message, polls it, and asserts the round-trip works end
// to end. The test failure surface is the wire layer — the
// in-process surfaces (decode + Dialer + poison FSM) are
// pinned in poller_kafka_test.go.
func TestKafkaPollerE2E_HappyPath(t *testing.T) {
	if os.Getenv("SKIP_KAFKA_E2E") != "" {
		t.Skip("SKIP_KAFKA_E2E set: skipping kafka e2e (no docker available)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Bring up Kafka. The image is the testcontainers default
	// (confluent-local); we override if KAFKA_TEST_IMAGE is set
	// (so CI can pin a specific version).
	img := os.Getenv("KAFKA_TEST_IMAGE")
	if img == "" {
		img = "confluentinc/confluent-local:7.5.0"
	}
	c, err := tckafka.Run(ctx, img)
	if err != nil {
		t.Skipf("testcontainers kafka not available (likely no Docker on host): %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })

	brokers, err := c.Brokers(ctx)
	if err != nil {
		t.Fatalf("kafka container brokers: %v", err)
	}
	if len(brokers) == 0 {
		t.Fatal("kafka container returned 0 brokers")
	}
	t.Logf("kafka container brokers = %v", brokers)

	const (
		topic = "faas-e2e-orders"
		group = "faas-e2e-group"
	)

	// First make the test producer quickly fail instead of
	// hanging if Kafka is unhealthy.
	dialer := &kafkago.Dialer{
		Timeout:   5 * time.Second,
		DualStack: true,
	}

	// Create the topic ahead of the publisher so the first
	// Send doesn't trigger a metadata roundtrip. CreateTopics
	// is idempotent — the "already exists" branch is logged
	// and the test continues.
	conn, err := dialer.DialContext(ctx, "tcp", brokers[0])
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := conn.CreateTopics(kafkago.TopicConfig{
		Topic:             topic,
		NumPartitions:     1,
		ReplicationFactor: 1,
	}); err != nil {
		t.Logf("create topic (idempotent): %v", err)
	}
	_ = conn.Close()

	writer := &kafkago.Writer{
		Addr:                   kafkago.TCP(brokers[0]),
		Topic:                  topic,
		Balancer:               &kafkago.LeastBytes{},
		AllowAutoTopicCreation: false,
		Transport: &kafkago.Transport{
			TLS: &tls.Config{MinVersion: tls.VersionTLS12}, // mirrors sched's pin
		},
		BatchTimeout: 50 * time.Millisecond,
	}
	if err := writer.WriteMessages(ctx, kafkago.Message{
		Key:   []byte("e2e-key-1"),
		Value: []byte(`{"order_id":42,"event":"order.created"}`),
		Headers: []kafkago.Header{
			{Key: "x-tenant", Value: []byte("acme")},
		},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Logf("writer close (non-fatal): %v", err)
	}

	// Build a sqlc.Trigger that the poller decodes. nil
	// FilterCriteria so the filter passes everything through
	// (filter_test.go pins the filter shape).
	triggerID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	cfgJSON := fmt.Sprintf(`{
		"brokers": %q,
		"topic":   %q,
		"group":   %q
	}`, brokers[0], topic, group)
	trigger := sqlc.Trigger{
		ID:     pgtypeUUIDFromUUID(t, triggerID),
		Kind:   "kafka",
		Config: []byte(cfgJSON),
		// FilterCriteria omitted (nil) — every record passes.
	}

	src, err := newKafkaPoller(trigger)
	if err != nil {
		t.Fatalf("new kafka poller: %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })

	// Poll once — expect a single SourceRecord with the
	// payload we just published.
	pollCtx, pollCancel := context.WithTimeout(ctx, 30*time.Second)
	defer pollCancel()
	res := src.Poll(pollCtx, trigger)
	if res.Error != nil {
		t.Fatalf("poller poll error: %v", res.Error)
	}
	if len(res.Records) != 1 {
		t.Fatalf("poller returned %d records, want 1", len(res.Records))
	}
	got := res.Records[0]
	if string(got.Payload) != `{"order_id":42,"event":"order.created"}` {
		t.Errorf("payload = %q, want order.created event body", got.Payload)
	}
	if got.Headers["x-tenant"] != "acme" {
		t.Errorf("header x-tenant = %q, want acme", got.Headers["x-tenant"])
	}

	// Ack — commits the offset so the next Poll returns
	// nothing.
	if err := src.Ack(ctx, trigger, []string{got.ItemIdentifier}); err != nil {
		t.Fatalf("Ack: %v", err)
	}

	// Re-poll — should be empty now.
	res = src.Poll(pollCtx, trigger)
	if len(res.Records) != 0 {
		t.Errorf("after Ack the second poll returned %d records, want 0", len(res.Records))
	}
}

// pgtypeUUIDFromUUID thinly wraps a google/uuid into the
// pgtype.UUID shape the sqlc.Trigger.ID field expects. The
// underlying 16-byte representation is the same wire shape
// as uuid.UUID.Bytes().
func pgtypeUUIDFromUUID(t *testing.T, u uuid.UUID) pgtype.UUID {
	t.Helper()
	bytes, err := u.MarshalBinary()
	if err != nil {
		t.Fatalf("uuid marshal: %v", err)
	}
	var pg pgtype.UUID
	pg.Bytes = [16]byte{}
	copy(pg.Bytes[:], bytes)
	pg.Valid = true
	return pg
}
