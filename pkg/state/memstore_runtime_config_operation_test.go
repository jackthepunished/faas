package state

import (
	"context"
	"encoding/json"
	"testing"
)

func TestMemStoreRuntimeConfigOperationLifecycle(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	row, err := store.UpsertRuntimeConfig(ctx, RuntimeConfigUpdate{
		Key: "request_read_timeout", Scope: RuntimeConfigScopeGlobal,
		DesiredValue: json.RawMessage(`"90s"`), ApplyMode: RuntimeConfigApplyGraceful,
		Reason: "test operation",
	})
	if err != nil {
		t.Fatalf("UpsertRuntimeConfig: %v", err)
	}
	op, err := store.CreateRuntimeConfigOperation(ctx, row, "actor", "test operation")
	if err != nil {
		t.Fatalf("CreateRuntimeConfigOperation: %v", err)
	}
	if op.Status != RuntimeConfigOperationPending || op.Phase != "queued" {
		t.Fatalf("new operation = %#v", op)
	}
	claimed, err := store.ClaimPendingRuntimeConfigOperation(ctx)
	if err != nil {
		t.Fatalf("ClaimPendingRuntimeConfigOperation: %v", err)
	}
	if claimed.Status != RuntimeConfigOperationRunning || claimed.StartedAt == nil {
		t.Fatalf("claimed operation = %#v", claimed)
	}
	if err := store.MarkRuntimeConfigOperationSucceeded(ctx, op.ID, op.DesiredValue, 1, 1); err != nil {
		t.Fatalf("MarkRuntimeConfigOperationSucceeded: %v", err)
	}
	got, err := store.GetRuntimeConfigOperation(ctx, op.ID)
	if err != nil {
		t.Fatalf("GetRuntimeConfigOperation: %v", err)
	}
	if got.Status != RuntimeConfigOperationSucceeded || got.FinishedAt == nil || string(got.EffectiveValue) != `"90s"` {
		t.Fatalf("terminal operation = %#v", got)
	}
	entry, err := store.GetRuntimeConfig(ctx, row.Key, row.Scope, row.ScopeID)
	if err != nil {
		t.Fatalf("GetRuntimeConfig after operation: %v", err)
	}
	if entry.Status != RuntimeConfigApplied || string(entry.EffectiveValue) != `"90s"` {
		t.Fatalf("effective config after operation = %#v", entry)
	}
}
