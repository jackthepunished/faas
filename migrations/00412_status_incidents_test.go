// migrations/00412_status_incidents_test.go — pins the shape of
// the status_incidents table (issue #599 / ADR-130 / cluster D
// commit 14 of the platform-observability mega-PR).

//go:build pg

package migrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func Test_00412_StatusIncidents(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	t.Run("insert open incident", func(t *testing.T) {
		var id int64
		err := pool.QueryRow(ctx,
			`INSERT INTO status_incidents (component, severity, message)
			 VALUES ('apid', 'degraded', 'login latency p99 > 5s')
			 RETURNING id`).Scan(&id)
		if err != nil {
			t.Fatalf("insert open incident: %v", err)
		}
		if id <= 0 {
			t.Errorf("id returned %d, want > 0", id)
		}
	})

	t.Run("component check rejects typo", func(t *testing.T) {
		// Out-of-vocabulary component must be rejected at
		// INSERT time (23514 check_violation). The CLI surface
		// relies on this — `gregale status incident post
		// --component=vmm` (typo) fails closed rather than
		// silently creating an un-grouped incident.
		_, err := pool.Exec(ctx,
			`INSERT INTO status_incidents (component, severity, message)
			 VALUES ('vmm', 'degraded', 'typo component')`)
		if err == nil {
			t.Fatal("typo component accepted; MUST be rejected by CHECK")
		}
		if !strings.Contains(err.Error(), "23514") {
			t.Errorf("expected 23514 check_violation; got %v", err)
		}
	})

	t.Run("severity check rejects typo", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO status_incidents (component, severity, message)
			 VALUES ('apid', 'mostly_down', 'typo severity')`)
		if err == nil {
			t.Fatal("typo severity accepted; MUST be rejected by CHECK")
		}
		if !strings.Contains(err.Error(), "23514") {
			t.Errorf("expected 23514 check_violation; got %v", err)
		}
	})

	t.Run("message length cap", func(t *testing.T) {
		// 1025-char message must be rejected.
		long := strings.Repeat("x", 1025)
		_, err := pool.Exec(ctx,
			`INSERT INTO status_incidents (component, severity, message)
			 VALUES ('apid', 'degraded', $1)`, long)
		if err == nil {
			t.Fatal("1025-char message accepted; MUST be rejected by CHECK")
		}
		if !strings.Contains(err.Error(), "23514") {
			t.Errorf("expected 23514 check_violation; got %v", err)
		}
	})

	t.Run("down removes table", func(t *testing.T) {
		if err := db.MigrateDown(ctx, pool); err != nil {
			t.Fatalf("migrate-down: %v", err)
		}
		var n int
		err := pool.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.tables
			 WHERE table_name = 'status_incidents'`).Scan(&n)
		if err != nil {
			t.Fatalf("table-existence check: %v", err)
		}
		if n != 0 {
			t.Errorf("status_incidents table still present after MigrateDown; got %d rows, want 0", n)
		}
		// Re-apply so post-test fixture is restored.
		if err := db.MigrateUp(ctx, pool); err != nil {
			t.Fatalf("re-migrate-up: %v", err)
		}
	})
}
