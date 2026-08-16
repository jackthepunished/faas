// dispatch_triggers_test.go — focused unit tests for the
// audit-round-2 fixes to pkg/sched/dispatch_triggers.go.
//
// Scope (audit round 2 finding #1, PR #910): the deadLetterAll
// helper now bridges the broker-handle namespace to the
// trigger_records.id UUID via TriggerRecordIDByItemIdentifier
// before calling InsertTriggerDeadLetter. The unit test pins:
//
//  1. happy path — record IS in trigger_records; lookup returns
//     the UUID; InsertTriggerDeadLetter + MarkTriggerRecordDeadLetter
//     both fire with the UUID (not the broker handle).
//  2. missing-row path — record is NOT in trigger_records yet
//     (rate-limit fired before InsertTriggerRecord); the helper
//     returns ("", nil); deadLetterAll SKIPS both store calls
//     for that item; no FK violation, broker offset stays put,
//     the record retries on the next dispatch tick.
//  3. poison_record path — caller passes a row UUID directly
//     (claimed[i].ID.String()); the lookup self-resolves and
//     the dead_letter row still lands.
//
// The fakeStore implements the storeLike interface from
// dispatch_triggers.go without any real SQL — every assertion
// is on the helper's call sequence, not on Postgres semantics.

package sched

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/state/sqlc"
)

// fakeDeadLetterStore is a minimal storeLike for the deadLetterAll
// unit tests. It tracks InsertTriggerDeadLetter + MarkTriggerRecordDeadLetter
// call sequences keyed by the record-id argument the helper passes
// in, so the tests can assert the helper resolved the broker
// handle to the trigger_records.id UUID before issuing writes.
type fakeDeadLetterStore struct {
	// records maps item_identifier -> trigger_records.id so the
	// TriggerRecordIDByItemIdentifier lookup has data to return.
	records map[string]string
	// inserts is the list of record-id arguments passed to
	// InsertTriggerDeadLetter, in call order.
	inserts []string
	// marks is the list of record-id arguments passed to
	// MarkTriggerRecordDeadLetter, in call order.
	marks []string
	// forceMissingIDs makes TriggerRecordIDByItemIdentifier
	// return ("", nil) for the listed item_identifiers
	// regardless of the records map.
	forceMissingIDs map[string]bool
}

func (f *fakeDeadLetterStore) ListEnabledTriggers(_ context.Context) ([]sqlc.Trigger, error) {
	return nil, nil
}
func (f *fakeDeadLetterStore) ClaimTriggerRecords(_ context.Context, _ string, _ int32) ([]sqlc.TriggerRecord, error) {
	return nil, nil
}
func (f *fakeDeadLetterStore) InsertTriggerRecord(_ context.Context, _, _ string, _, _, _ []byte) (string, error) {
	return "", nil
}
func (f *fakeDeadLetterStore) MarkTriggerRecordSucceeded(_ context.Context, _ string) error {
	return nil
}
func (f *fakeDeadLetterStore) MarkTriggerRecordRetry(_ context.Context, _, _ string, _ time.Time) error {
	return errors.New("not used in this test")
}
func (f *fakeDeadLetterStore) MarkTriggerRecordDeadLetter(_ context.Context, id, _ string) error {
	f.marks = append(f.marks, id)
	return nil
}
func (f *fakeDeadLetterStore) InsertTriggerDeadLetter(_ context.Context, recordID, _, _, _ string, _ []byte) error {
	f.inserts = append(f.inserts, recordID)
	return nil
}
func (f *fakeDeadLetterStore) TriggerRecordIDByItemIdentifier(_ context.Context, _, itemIdentifier string) (string, error) {
	if f.forceMissingIDs[itemIdentifier] {
		return "", nil
	}
	if uuid, ok := f.records[itemIdentifier]; ok {
		return uuid, nil
	}
	return "", nil
}

// makeLoopForDLQ builds a Loop with just enough wiring for
// deadLetterAll — log + nil engine — so the test exercises the
// helper directly without the full dispatchOneTrigger surface.
func makeLoopForDLQ() *Loop {
	return &Loop{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// TestDeadLetterAll_HappyPath_BrokerHandleResolvesToUUID verifies
// the helper bridges the broker-handle namespace to the
// trigger_records.id UUID before issuing the dead_letter writes.
// Audit round 2 finding #1 (PR #910): without the lookup every
// rate-limit denial tripped SQLSTATE 23503 because
// trigger_dead_letter.record_id is a UUID FK into
// trigger_records.id.
func TestDeadLetterAll_HappyPath_BrokerHandleResolvesToUUID(t *testing.T) {
	const (
		triggerID = "11111111-1111-1111-1111-111111111111"
		kafkaOff  = "kafka-offset-12"
		rowUUID   = "22222222-2222-2222-2222-222222222222"
	)
	store := &fakeDeadLetterStore{
		records: map[string]string{kafkaOff: rowUUID},
	}
	l := makeLoopForDLQ()
	l.deadLetterAll(context.Background(), triggerID, []string{kafkaOff},
		triggerReasonRateLimited, "wake rate limit exceeded", store)

	if got, want := len(store.inserts), 1; got != want {
		t.Fatalf("InsertTriggerDeadLetter call count = %d, want %d (errors=%v)", got, want, store.inserts)
	}
	if got, want := store.inserts[0], rowUUID; got != want {
		t.Errorf("InsertTriggerDeadLetter record_id = %q, want %q (must be the UUID, not the broker handle)", got, want)
	}
	if got, want := len(store.marks), 1; got != want {
		t.Fatalf("MarkTriggerRecordDeadLetter call count = %d, want %d", got, want)
	}
	if got, want := store.marks[0], rowUUID; got != want {
		t.Errorf("MarkTriggerRecordDeadLetter id = %q, want %q (must be the UUID, not the broker handle)", got, want)
	}
}

// TestDeadLetterAll_RowMissing_RateLimitBeforeInsert pins the
// "rate-limit fires before InsertTriggerRecord" path. The
// trigger_records row does not exist yet (the insert happens
// later in dispatchOneTrigger); the lookup returns ("", nil);
// deadLetterAll MUST skip both store calls for that item so the
// broker offset stays put and the record retries on the next
// tick. Without the skip, the old code would have tripped an FK
// violation, the record would have stayed in poller.inFlight
// forever, and the broker offset would never advance.
func TestDeadLetterAll_RowMissing_RateLimitBeforeInsert(t *testing.T) {
	const (
		triggerID = "11111111-1111-1111-1111-111111111111"
		kafkaOff  = "kafka-offset-99"
	)
	store := &fakeDeadLetterStore{
		records:         map[string]string{}, // empty — no row
		forceMissingIDs: map[string]bool{kafkaOff: true},
	}
	l := makeLoopForDLQ()
	l.deadLetterAll(context.Background(), triggerID, []string{kafkaOff},
		triggerReasonRateLimited, "wake rate limit exceeded", store)

	if got := len(store.inserts); got != 0 {
		t.Errorf("InsertTriggerDeadLetter call count = %d, want 0 (rate-limited records must retry, not be silently dropped)", got)
	}
	if got := len(store.marks); got != 0 {
		t.Errorf("MarkTriggerRecordDeadLetter call count = %d, want 0 (no row to mark)", got)
	}
}

// TestDeadLetterAll_PoisonPath_PassesUUIDs pins the
// poison_record path at dispatch_triggers.go:354 — the caller
// already has the trigger_records.id UUID (from the `claimed`
// result-set, line 352), so the lookup self-resolves and the
// dead_letter row still lands. This guards against a future
// refactor that might "optimise" by skipping the lookup when the
// input looks UUID-like — the helper has no such heuristic; it
// always goes through TriggerRecordIDByItemIdentifier.
func TestDeadLetterAll_PoisonPath_PassesUUIDs(t *testing.T) {
	const (
		triggerID = "11111111-1111-1111-1111-111111111111"
		rowUUID   = "33333333-3333-3333-3333-333333333333"
	)
	store := &fakeDeadLetterStore{
		records: map[string]string{rowUUID: rowUUID}, // self-resolves
	}
	l := makeLoopForDLQ()
	l.deadLetterAll(context.Background(), triggerID, []string{rowUUID},
		triggerReasonPoisonRecord, "gateway response malformed", store)

	if got, want := len(store.inserts), 1; got != want {
		t.Fatalf("InsertTriggerDeadLetter call count = %d, want %d", got, want)
	}
	if got, want := store.inserts[0], rowUUID; got != want {
		t.Errorf("InsertTriggerDeadLetter record_id = %q, want %q", got, want)
	}
	if got, want := len(store.marks), 1; got != want {
		t.Fatalf("MarkTriggerRecordDeadLetter call count = %d, want %d", got, want)
	}
	if got, want := store.marks[0], rowUUID; got != want {
		t.Errorf("MarkTriggerRecordDeadLetter id = %q, want %q", got, want)
	}
}

// TestDeadLetterAll_MixedPath_SkipsOnlyMissingRows pins the
// partial-success case: one item_identifier resolves to a UUID,
// another doesn't. The helper must insert + mark the resolved
// row AND skip the unresolved one — never both, never neither.
func TestDeadLetterAll_MixedPath_SkipsOnlyMissingRows(t *testing.T) {
	const (
		triggerID = "11111111-1111-1111-1111-111111111111"
		present   = "present-handle"
		missing   = "missing-handle"
		rowUUID   = "44444444-4444-4444-4444-444444444444"
	)
	store := &fakeDeadLetterStore{
		records:         map[string]string{present: rowUUID},
		forceMissingIDs: map[string]bool{missing: true},
	}
	l := makeLoopForDLQ()
	l.deadLetterAll(context.Background(), triggerID, []string{present, missing},
		triggerReasonRateLimited, "wake rate limit exceeded", store)

	if got, want := len(store.inserts), 1; got != want {
		t.Fatalf("InsertTriggerDeadLetter call count = %d, want %d (mixed: 1 row, 1 missing)", got, want)
	}
	if got, want := store.inserts[0], rowUUID; got != want {
		t.Errorf("InsertTriggerDeadLetter record_id = %q, want %q", got, want)
	}
	if got, want := len(store.marks), 1; got != want {
		t.Fatalf("MarkTriggerRecordDeadLetter call count = %d, want %d", got, want)
	}
	if got, want := store.marks[0], rowUUID; got != want {
		t.Errorf("MarkTriggerRecordDeadLetter id = %q, want %q", got, want)
	}
}
