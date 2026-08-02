// memstore_wake_id_test.go — issue #517 / PR-C, ADR-064 — the
// in-memory twin of the production read-side wake-timeline
// query. Pins the contract so the customer-facing endpoint's
// unit tests can drive a real store without standing up Postgres.
package state

import (
	"context"
	"testing"
	"time"
)

// TestMemStore_ListEventsByWakeID (issue #517 / PR-C) — the
// MemStore twin of the production ListEventsByWakeID query.
// Filters on jsonb data.wake_id (mimicking the partial index
// shape), orders forward (oldest → newest), and respects the
// since lower bound + limit cap. Mirrors the sqlc query body
// in pkg/state/queries.sql::ListEventsByWakeID.
func TestMemStore_ListEventsByWakeID(t *testing.T) {
	m := NewMemStore()
	ctx := context.Background()
	// Three wakes, each with three phases; one orphan row whose
	// data lacks wake_id. The query must return the three
	// wake_id="w-1" rows in at-ASC order.
	for i, kind := range []string{"wake.queue_accepted", "wake.boot_started", "wake.boot_completed"} {
		_ = m.AppendEvent(ctx, "schedd", kind, nil, []byte(`{"wake_id":"w-1","app_id":"a-1"}`))
		_ = i
		time.Sleep(1 * time.Millisecond) // keep at strictly increasing
	}
	for _, kind := range []string{"wake.queue_accepted", "wake.boot_started"} {
		_ = m.AppendEvent(ctx, "schedd", kind, nil, []byte(`{"wake_id":"w-2","app_id":"a-2"}`))
		time.Sleep(1 * time.Millisecond)
	}
	// Orphan row (no wake_id) — must NOT appear in any
	// ListEventsByWakeID result.
	_ = m.AppendEvent(ctx, "schedd", "legacy", nil, []byte(`{"actor":"manual"}`))

	// All rows for w-1, no since filter, no limit.
	got, err := m.ListEventsByWakeID(ctx, "w-1", time.Time{}, 0)
	if err != nil {
		t.Fatalf("ListEventsByWakeID: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3 (queue_accepted, boot_started, boot_completed)", len(got))
	}
	if got[0].Kind != "wake.queue_accepted" {
		t.Errorf("got[0].Kind = %q, want wake.queue_accepted (ASC order)", got[0].Kind)
	}
	if got[2].Kind != "wake.boot_completed" {
		t.Errorf("got[2].Kind = %q, want wake.boot_completed (ASC order)", got[2].Kind)
	}

	// Limit cap.
	limited, err := m.ListEventsByWakeID(ctx, "w-1", time.Time{}, 2)
	if err != nil {
		t.Fatalf("ListEventsByWakeID limited: %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("limited = %d, want 2", len(limited))
	}

	// Since lower bound excludes the first row.
	since := got[0].At.Add(1 * time.Microsecond)
	after, err := m.ListEventsByWakeID(ctx, "w-1", since, 0)
	if err != nil {
		t.Fatalf("ListEventsByWakeID since: %v", err)
	}
	if len(after) != 2 {
		t.Errorf("after since = %d, want 2", len(after))
	}

	// Unknown wake_id → empty.
	none, err := m.ListEventsByWakeID(ctx, "w-unknown", time.Time{}, 0)
	if err != nil {
		t.Fatalf("ListEventsByWakeID unknown: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("unknown wake_id = %d, want 0", len(none))
	}
}
