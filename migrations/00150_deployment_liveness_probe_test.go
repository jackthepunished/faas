//go:build !no_pg

// Migration-apply test for 00150 (issue #554 / ADR-078 follow-up —
// per-deployment liveness-probe override jsonb column). The
// migration is additive + nullable + coalesce-defaults to NULL,
// matching the existing override_healthcheck shape
// (migrations/00079_deployment_overrides.sql). This test pins:
//
//  1. The migration set applies cleanly through 00150.
//  2. deployments.override_liveness_probe is a jsonb column on
//     the table (NOT NULL is the default for jsonb on this
//     schema; INSERT with no value coalesces to NULL on the
//     read side).
//  3. Replay-safety: a second MigrateUp is a no-op.
//
// Slot note: 00149 is the issue #554 slot fence
// (migrations/00149_reserve_slot.sql); 00150 is the first
// follow-up column claimed from the fence's "if a follow-up
// lands deployments.parked_reason text" note. We did NOT land
// parked_reason — instead we landed override_liveness_probe,
// the column the vmmd liveness poll goroutine actually consumes.
// The parked_reason surface is exposed via
// `instances.parked_liveness_exhausted` audit events (operator
// greps `kind_prefix=instances.parked_liveness_*`).
package migrations_test

import (
	"context"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00150_DeploymentLivenessProbe(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// (1) Apply through 00150. A regression that drops a slot
	// between 1 and 149 surfaces here before the structural pin.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 1 and 149)", err)
	}

	// (2) override_liveness_probe column exists + round-trips.
	// We can't create a deployment without an app; verify the
	// column exists via information_schema (the column-add
	// migration is the load-bearing path here).
	var dataType *string
	if err := pool.QueryRow(ctx,
		`SELECT data_type FROM information_schema.columns
		 WHERE table_schema = current_schema()
		   AND table_name   = 'deployments'
		   AND column_name  = 'override_liveness_probe'`,
	).Scan(&dataType); err != nil {
		t.Fatalf("select information_schema.columns: %v", err)
	}
	if dataType == nil || *dataType != "jsonb" {
		t.Errorf("deployments.override_liveness_probe data_type = %v, want \"jsonb\"", dataType)
	}

	// (3) Replay safety — a second MigrateUp is a no-op. Goose's
	// goose_db_version row pins the state.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay safety: second MigrateUp returned error: %v", err)
	}
}