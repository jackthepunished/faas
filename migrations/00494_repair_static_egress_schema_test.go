//go:build !no_pg

// Regression test for 00494_repair_static_egress_schema.sql.
//
// It models the production failure: goose has already recorded 00336 and
// 00337, while the schema objects owned by those historical slots are absent.
// The append-only repair must restore the objects when only version 00494 is
// pending, and a second run must be a no-op.
package migrations_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00494RepairsStaticEgressSchemaDrift(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("initial db.MigrateUp: %v", err)
	}

	assertStaticEgressSchema(t, ctx, pool)

	// Keep the historical ledger entries in place. This is the important
	// detail: 00336 and 00337 are already applied, so goose will not replay
	// their now-correct source files.
	var applied int
	if err := pool.QueryRow(ctx, `
		select count(*)
		  from goose_db_version
		 where version_id in (336, 337)`).Scan(&applied); err != nil {
		t.Fatalf("check historical migration ledger rows: %v", err)
	}
	if applied != 2 {
		t.Fatalf("historical static-egress migration rows = %d, want 2", applied)
	}

	// Recreate the deployed drift: the ledger says the feature is present,
	// but its schema objects are missing. The later migration owns no data in
	// this isolated test, so removing the objects is safe and deterministic.
	if _, err := pool.Exec(ctx, `
		drop index if exists apps_static_egress_ip_key;
		alter table apps drop constraint if exists apps_static_egress_ip_family_check;
		alter table apps drop column if exists static_egress_ip_set_at;
		alter table apps drop column if exists static_egress_ip;
		drop table if exists provisioned_static_egress_ips`); err != nil {
		t.Fatalf("create drifted schema: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		delete from goose_db_version
		 where version_id = 494`); err != nil {
		t.Fatalf("make repair migration pending: %v", err)
	}

	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("repair db.MigrateUp: %v", err)
	}
	assertStaticEgressSchema(t, ctx, pool)

	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay db.MigrateUp: %v", err)
	}
}

func assertStaticEgressSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()

	for _, column := range []string{"static_egress_ip", "static_egress_ip_set_at"} {
		var exists bool
		if err := pool.QueryRow(ctx, `
			select exists (
				select 1
				  from information_schema.columns
				 where table_schema = current_schema()
				   and table_name = 'apps'
				   and column_name = $1
			)`, column).Scan(&exists); err != nil {
			t.Fatalf("check apps.%s: %v", column, err)
		}
		if !exists {
			t.Errorf("apps.%s is missing", column)
		}
	}

	var appsIndex, appsCheck, tableExists, customerIPIndex bool
	if err := pool.QueryRow(ctx, `
		select
			exists (
				select 1 from pg_indexes
				 where schemaname = current_schema()
				   and tablename = 'apps'
				   and indexname = 'apps_static_egress_ip_key'
			),
			exists (
				select 1
				  from pg_constraint c
				  join pg_class t on t.oid = c.conrelid
				 where t.relnamespace = current_schema()::regnamespace
				   and t.relname = 'apps'
				   and c.conname = 'apps_static_egress_ip_family_check'
			),
			exists (
				select 1
				  from pg_class
				 where relnamespace = current_schema()::regnamespace
				   and relname = 'provisioned_static_egress_ips'
			),
			exists (
				select 1 from pg_indexes
				 where schemaname = current_schema()
				   and tablename = 'provisioned_static_egress_ips'
				   and indexname = 'provisioned_static_egress_ips_customer_ip_idx'
			)`).Scan(&appsIndex, &appsCheck, &tableExists, &customerIPIndex); err != nil {
		t.Fatalf("check static-egress indexes and constraints: %v", err)
	}
	if !appsIndex {
		t.Error("apps_static_egress_ip_key is missing")
	}
	if !appsCheck {
		t.Error("apps_static_egress_ip_family_check is missing")
	}
	if !tableExists {
		t.Error("provisioned_static_egress_ips is missing")
	}
	if !customerIPIndex {
		t.Error("provisioned_static_egress_ips_customer_ip_idx is missing")
	}
}
