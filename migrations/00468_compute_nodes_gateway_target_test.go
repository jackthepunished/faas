//go:build !no_pg

package migrations_test

import (
	"context"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigration_00468ComputeNodeGatewayTarget(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	defer pool.Close()
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}

	var columnCount int
	if err := pool.QueryRow(ctx, `
		select count(*)
		  from information_schema.columns
		 where table_schema = current_schema()
		   and table_name = 'compute_nodes'
		   and column_name = 'gateway_target_url'
	`).Scan(&columnCount); err != nil {
		t.Fatalf("query gateway_target_url column: %v", err)
	}
	if columnCount != 1 {
		t.Fatalf("gateway_target_url column count = %d, want 1", columnCount)
	}

	var constraintCount int
	if err := pool.QueryRow(ctx, `
		select count(*)
		  from pg_constraint
		 where conrelid = 'compute_nodes'::regclass
		   and conname = 'compute_nodes_gateway_target_url_scheme_chk'
	`).Scan(&constraintCount); err != nil {
		t.Fatalf("query gateway target constraint: %v", err)
	}
	if constraintCount != 1 {
		t.Fatalf("gateway target constraint count = %d, want 1", constraintCount)
	}
}
