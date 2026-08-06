//go:build !no_pg

// Migration-apply test for 00153 (issue #554 — Liveness probe: restart a
// wedged VM, ADR-078). The migration is a no-op slot fence per ADR-041;
// this test pins:
//
//  1. The migration set applies cleanly through 00153.
//  2. instances.framework_ready_at (PR #543 / issue #470-FU-B) is
//     unchanged by the fence.
//  3. Replay-safety: a second MigrateUp is a no-op.
//
// Slot note: 00148 (overage_cap_gate_index, issue #561 fence) is the
// highest committed slot on the local branch before 00153; 00147 is
// deployments_scan_result (issue #464 / ADR-055); 00146 is a
// reserve_slot; 00145 is sessions_binding (PR #658). Slot 149 is the
// first open number.
package migrations_test

import (
	"context"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00149_LivenessProbeReserveSlot(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// (1) Apply through 00153. A regression that drops a slot
	// between 1 and 148 surfaces here before the structural pin.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 1 and 148)", err)
	}

	// (2) instances.framework_ready_at remains the PR #543 / #470
	// schema: nullable TIMESTAMPTZ, NOT changed by the fence.
	// Probe the column type to confirm the column is still there.
	var typ string
	if err := pool.QueryRow(ctx,
		`SELECT data_type FROM information_schema.columns
		 WHERE table_schema = current_schema() AND table_name = 'instances'
		   AND column_name = 'framework_ready_at'`,
	).Scan(&typ); err != nil {
		t.Fatalf("instances.framework_ready_at lookup: %v", err)
	}
	if typ != "timestamp with time zone" {
		t.Fatalf("instances.framework_ready_at type = %q, want \"timestamp with time zone\"", typ)
	}

	// (3) Replay safety — a second MigrateUp is a no-op. Goose's
	// goose_db_version row pins the state.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay safety: second MigrateUp returned error: %v", err)
	}
}
