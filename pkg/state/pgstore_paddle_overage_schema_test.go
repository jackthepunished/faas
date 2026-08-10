// pgstore_paddle_overage_schema_test.go — real-Postgres pins of the
// B4 pre-flight (PaddleOverageDedupeSchema). The companion memstore
// file (memstore_paddle_overage_schema_test.go) covers the
// in-process shape; this file covers the wire shape — what an
// apid process actually sees when it boots against the live
// control-plane Postgres.
//
// The CLI maps the probe output to operator-facing hints:
//   TableExists=false               → "apply 00034 then 00041"
//   TableExists=true, any HasX=false → "apply 00041"
//   everything true                  → ready
//
// A regression that flips either branch (e.g. a future migration
// that drops the 00041 columns, or a refactor that lets the probe
// short-circuit on the column query without surfacing the missing
// state) would flip these tests red. The pg-shard CI job owns
// them; no FAAS_PADDLE_API_KEY is required.
//
// Build tag mirrors the rest of pkg/state pgtests: !no_pg so the
// FAAS_SKIP_PG_TESTS=1 escape hatch still works.

package state

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

// TestPgStorePaddleOverageDedupeSchema_PostApply is the happy
// path: after migrations 00034 + 00041 + 00198 apply, the probe
// reports the four 00041 columns + non-zero counts. The two
// claims + one complete seed mirrors what meterd produces in
// production over a two-window stretch.
func TestPgStorePaddleOverageDedupeSchema_PostApply(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}

	s := NewPgStore(pool)

	// Seed two distinct (acct, window) pairs, then complete one.
	// Email must be unique (accounts.email is the natural key).
	acctA := uniqueAcctID("pg-schema-A")
	acctB := uniqueAcctID("pg-schema-B")
	if _, err := s.CreateAccount(ctx, acctA+"@mig.example.test", api.PlanFree); err != nil {
		t.Fatalf("CreateAccount(%s): %v", acctA, err)
	}
	if _, err := s.CreateAccount(ctx, acctB+"@mig.example.test", api.PlanFree); err != nil {
		t.Fatalf("CreateAccount(%s): %v", acctB, err)
	}

	now := time.Now().UTC().Truncate(time.Hour)
	windowA := now
	windowB := now.Add(time.Hour)
	lease := 5 * time.Minute

	if claimed, err := s.ClaimPaddleOverageWindow(ctx, acctA, windowA, "pod-A", lease); err != nil || !claimed {
		t.Fatalf("claim A: claimed=%v err=%v", claimed, err)
	}
	if claimed, err := s.ClaimPaddleOverageWindow(ctx, acctB, windowB, "pod-B", lease); err != nil || !claimed {
		t.Fatalf("claim B: claimed=%v err=%v", claimed, err)
	}
	if err := s.CompletePaddleOverageWindow(ctx, acctB, windowB, 100); err != nil {
		t.Fatalf("Complete B: %v", err)
	}

	res, err := s.PaddleOverageDedupeSchema(ctx)
	if err != nil {
		t.Fatalf("PaddleOverageDedupeSchema: %v", err)
	}
	if !res.TableExists {
		t.Errorf("TableExists = false post-apply, want true (migrations 00034 + 00041 + 00198 must have created paddle_overage_dedupe)")
	}
	if !res.HasWindowStart || !res.HasState || !res.HasClaimedAt || !res.HasClaimedBy {
		t.Errorf("all HasX must be true post-apply; got %+v (a future migration that drops one of the 00041 columns would flip this red)", res)
	}
	if res.PendingRows != 1 {
		t.Errorf("PendingRows = %d, want 1 (acctA@windowA is still pending)", res.PendingRows)
	}
	if res.CompletedRows != 1 {
		t.Errorf("CompletedRows = %d, want 1 (acctB@windowB was completed with mbSeconds=100)", res.CompletedRows)
	}
}

// TestPgStorePaddleOverageDedupeSchema_PreApply_ReturnsTableMissing
// is the tripwire the B4 CLI maps to "apply 00034 then 00041".
// After the migrate-up, drop the table directly and re-probe.
// The probe must surface TableExists=false (and all HasX=false
// and counts zero) so the operator gets the missing-table hint
// rather than a 500 / generic probe error.
func TestPgStorePaddleOverageDedupeSchema_PreApply_ReturnsTableMissing(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}

	if _, err := pool.Exec(ctx, `drop table if exists paddle_overage_dedupe`); err != nil {
		t.Fatalf("drop paddle_overage_dedupe: %v", err)
	}

	s := NewPgStore(pool)
	res, err := s.PaddleOverageDedupeSchema(ctx)
	if err != nil {
		t.Fatalf("PaddleOverageDedupeSchema on missing table: %v (probe must surface TableExists=false, not a raw error)", err)
	}
	if res.TableExists {
		t.Errorf("TableExists = true after DROP, want false (the to_regclass probe must report the missing table — the CLI maps this to the missing-table hint)")
	}
	if res.HasWindowStart || res.HasState || res.HasClaimedAt || res.HasClaimedBy {
		t.Errorf("all HasX must be false on missing table; got %+v", res)
	}
	if res.PendingRows != 0 || res.CompletedRows != 0 {
		t.Errorf("counts on missing table must be 0; got pending=%d completed=%d", res.PendingRows, res.CompletedRows)
	}
}

// uniqueAcctID produces a per-test account id that won't collide
// with another test's seed. accountID is the natural primary key
// (state.accounts.id) so the test harness relies on a unique
// value, not a unique email. The unix-nano suffix is the same
// pattern cmd/e2e/billing_paddle_sandbox_test.go:250 uses for
// the signup email — keeping both seams consistent.
func uniqueAcctID(label string) string {
	return fmt.Sprintf("acct-%s-%d", label, time.Now().UnixNano())
}
