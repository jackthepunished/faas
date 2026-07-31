// Phase 5 repo decomposition (PR-E): pgstore UpdateApp widening. The
// apps table already carries root_dir / workload_name / start_command
// (migration 00074); the widening makes them mutable through the
// PATCH path so pkg/reconcile can stamp fresh workload identity on
// every changed app. The MemStore mirror lives in memstore_test.go
// (TestMemStore_UpdateApp_AllFields_Sets tripwire). These tests pin
// the partial-update + round-trip semantics on PgStore.
//
//go:build !no_pg

package state_test

import (
	"errors"
	"testing"

	"github.com/onebox-faas/faas/pkg/state"
)

// TestPg_UpdateApp_WorkloadFields_Widen pins the partial-update
// semantics of the three widened fields:
//
//   - RootDir, WorkloadName: NOT NULL DEFAULT ”, so nil=leave-alone,
//     non-nil=verbatim copy (empty string = reset to default).
//   - StartCommand: nullable, so nil=leave-alone, empty string is
//     mapped to NULL via the nullString helper.
//
// The test mirrors TestPg_UpdateApp_WithMinInstances / TestPg_UpdateApp_AutoscaleTargets:
// a regression that drops one of the widened fields from the SQL
// UPDATE or RETURNING trips the round-trip assertions immediately.
func TestPg_UpdateApp_WorkloadFields_Widen(t *testing.T) {
	s, ctx := pgStore(t)
	_, appID, _ := seedLiveDeploy(t, s, ctx)

	// Seed baseline via direct UPDATE so the "unset must survive"
	// cases below have something to verify against.
	rootDir := "services/api"
	workloadName := "api"
	startCmd := "node server.js"
	a, err := s.UpdateApp(ctx, appID, state.UpdateAppParams{
		RootDir:      &rootDir,
		WorkloadName: &workloadName,
		StartCommand: &startCmd,
	})
	if err != nil {
		t.Fatalf("UpdateApp initial: %v", err)
	}
	if a.RootDir != rootDir || a.WorkloadName != workloadName || a.StartCommand != startCmd {
		t.Fatalf("after Set: root=%q workload=%q start=%q", a.RootDir, a.WorkloadName, a.StartCommand)
	}

	// Unset on every widened field must leave the columns alone
	// (mirrors the SetMinInstances / SetAutoscaleTargetRPS contract).
	a, err = s.UpdateApp(ctx, appID, state.UpdateAppParams{})
	if err != nil {
		t.Fatalf("UpdateApp unset: %v", err)
	}
	if a.RootDir != rootDir || a.WorkloadName != workloadName || a.StartCommand != startCmd {
		t.Fatalf("unset survival: got root=%q workload=%q start=%q", a.RootDir, a.WorkloadName, a.StartCommand)
	}

	// Explicit change on one field, unset on the others. Only the
	// set field should move.
	newRoot := "services/worker"
	a, err = s.UpdateApp(ctx, appID, state.UpdateAppParams{
		RootDir: &newRoot,
	})
	if err != nil {
		t.Fatalf("UpdateApp newRoot: %v", err)
	}
	if a.RootDir != newRoot {
		t.Errorf("RootDir after single-set: got %q, want %q", a.RootDir, newRoot)
	}
	if a.WorkloadName != workloadName || a.StartCommand != startCmd {
		t.Errorf("non-target fields drifted: workload=%q start=%q", a.WorkloadName, a.StartCommand)
	}

	// StartCommand explicit-empty round-trip → NULL (the nullString
	// helper maps "" to nil).
	empty := ""
	a, err = s.UpdateApp(ctx, appID, state.UpdateAppParams{
		StartCommand: &empty,
	})
	if err != nil {
		t.Fatalf("UpdateApp start-empty: %v", err)
	}
	if a.StartCommand != "" {
		t.Errorf("StartCommand empty: got %q, want \"\"", a.StartCommand)
	}

	// UpdateApp on a missing app must return ErrNotFound, same shape
	// as the other UpdateApp tests. The apps.id column is UUID, so the
	// missing-id must be syntactically valid (otherwise pgx trips the
	// cast at SQLSTATE 22P02 before the not-found check can fire).
	if _, err := s.UpdateApp(ctx, "00000000-0000-0000-0000-000000000000", state.UpdateAppParams{
		RootDir: &rootDir,
	}); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("missing app update: got %v, want ErrNotFound", err)
	}
}
