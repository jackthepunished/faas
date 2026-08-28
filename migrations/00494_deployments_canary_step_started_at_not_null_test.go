// migrations/00494_deployments_canary_step_started_at_not_null_test.go —
// pins the SAFE-RELEASES code-review hardening of the canary
// wall-clock gate. Migration 00494 backfills NULL
// canary_step_started_at to COALESCE(created_at, NOW()), locks the
// column as NOT NULL, and stamps a NOW() default so future INSERT
// paths that omit the column still get a meaningful (non-zero)
// timestamp. Closes the silent-soak-bypass hole exposed by
// code-review finding #1 (pkg/canary.Once:226) and finding #2
// (pkg/safedeploy.Orchestrator:207). Build tag mirrors the
// precedent at migrations/00410_app_secret_value_hash_test.go;
// set FAAS_SKIP_PG_TESTS=1 locally to skip.
//
// Asserts:
//   1. The migration set applies cleanly through 00494 (and lands
//      00494 last).
//   2. The canary_step_started_at column is NOT NULL with a NOW()
//      DEFAULT — the DEFAULT is the belt-and-braces that keeps
//      the wall-clock gate honest when a future write path forgets
//      to stamp the timestamp.
//   3. INSERTing a deployment with canary_step_started_at IS NULL
//      fails with SQLSTATE 23502 (NOT NULL violation) — explicit
//      NULL is rejected even though DEFAULT exists.
//   4. INSERTing a deployment without specifying canary_step_started_at
//      succeeds (the DEFAULT fires) and lands with a non-zero
//      timestamp.
//   5. Replay safety — a second MigrateUp is a no-op.
//
//go:build pg

package migrations_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00494_CanaryStepStartedAtNotNull(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// (1) Run the full migration set. 00494 should land last.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (PR follow-up failure mode: missing migration slot before 00494)", err)
	}

	// (2) Column nullable rule + DEFAULT presence on canary_step_started_at.
	var nullable string
	var def *string
	err := pool.QueryRow(ctx, `
		select is_nullable, column_default
		  from information_schema.columns
		 where table_schema = current_schema()
		   and table_name = 'deployments'
		   and column_name = 'canary_step_started_at'`).Scan(&nullable, &def)
	if err != nil {
		t.Fatalf("query canary_step_started_at is_nullable/column_default: %v", err)
	}
	if nullable != "NO" {
		t.Errorf("deployments.canary_step_started_at nullable = %q, want NO (NOT NULL — code-review finding #1/#2 hardening)", nullable)
	}
	if def == nil || *def == "" {
		t.Errorf("deployments.canary_step_started_at column_default = nil, want NOW() default (belt-and-braces for write paths that forget to stamp)")
	} else if !strings.Contains(strings.ToLower(*def), "now") {
		t.Errorf("deployments.canary_step_started_at column_default = %q, want a NOW()-family default", *def)
	}

	// (3) An INSERT with canary_step_started_at IS NULL must fail
	// with SQLSTATE 23502 (NOT NULL violation) — proves the column
	// is now locked at the schema layer even though a DEFAULT exists.
	_, insertErr := pool.Exec(ctx, `
		insert into deployments (app_id, account_id, status, source_kind, commit_sha, canary_step_started_at)
		values ($1, $2, 'live', 'git', 'def5678', NULL)`,
		uuid.New().String(), uuid.New().String())
	if insertErr == nil {
		t.Errorf("insert with NULL canary_step_started_at must fail post-00494 (NOT NULL violation); got nil")
	}
	// pgtest surfaces the SQLSTATE in err.Error() — 23502 is the
	// standard NOT NULL violation. We don't pin the exact error
	// string (PG minor versions vary), only that the column name
	// shows up so an operator knows what to fix.
	if !strings.Contains(insertErr.Error(), "23502") &&
		!strings.Contains(insertErr.Error(), "canary_step_started_at") {
		t.Errorf("insert error = %v; want SQLSTATE 23502 or 'canary_step_started_at' in the message", insertErr)
	}

	// (4) An INSERT without specifying canary_step_started_at
	// succeeds (DEFAULT NOW() fires) and lands with a non-zero
	// timestamp — proves the write-path-safety DEFAULT.
	var landedAt time.Time
	landErr := pool.QueryRow(ctx, `
		insert into deployments (app_id, account_id, status, source_kind, commit_sha)
		values ($1, $2, 'live', 'git', 'happy01')
		returning canary_step_started_at`,
		uuid.New().String(), uuid.New().String()).Scan(&landedAt)
	if landErr != nil {
		t.Fatalf("insert without canary_step_started_at (DEFAULT NOW() expected): %v", landErr)
	}
	if landedAt.IsZero() {
		t.Errorf("DEFAULT-stamped canary_step_started_at is zero time — wall-clock gate would silently bypass; want NOW()")
	}
	if delta := time.Since(landedAt); delta < -2*time.Second || delta > 2*time.Second {
		t.Errorf("DEFAULT-stamped canary_step_started_at = %v, want within 2s of now() (delta=%v)", landedAt, delta)
	}

	// (5) Replay safety — a second MigrateUp must be a no-op.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay db.MigrateUp: %v (the COALESCE UPDATE WHERE ... IS NULL + ALTER ... SET NOT NULL must silently no-op)", err)
	}
}
