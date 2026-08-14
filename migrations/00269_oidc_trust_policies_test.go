//go:build !no_pg

// Migration-apply test for 00269 (oidc_trust_policies). Pins
// the load-bearing PR-A schema contract:
//
//   1. The migration applies cleanly through 00269.
//   2. All 10 columns are present with the right types and NOT NULL
//      constraints.
//   3. The composite PK is (account_id, issuer_url).
//   4. The FK to accounts(id) accepts a real account and rejects
//      a foreign one (23503).
//   5. ON DELETE CASCADE: deleting the owning account removes the
//      trust policy row (PR-A's GDPR §17 G2 path).
//   6. Re-applying the migration is a no-op (IF NOT EXISTS).
//
// Build tag mirrors apply_walk_test.go:4.

package migrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00269_OIDCTrustPolicies(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}

	// (1) All 10 columns present with expected types. Postgres
	// reports timestamptz as "timestamp with time zone" in
	// information_schema (the SQL-standard name); the
	// "timestamptz" alias is a parser shortcut, not the storage
	// type. Same caveat as
	// pg_get_constraintdef (MEMORY.md/pg-get-constraintdef-shapes).
	type colSpec struct {
		name    string
		typ     string
		notnull bool
	}
	wantCols := []colSpec{
		{"account_id", "uuid", true},
		{"issuer_url", "text", true},
		{"jwks_url", "text", true},
		{"audience", "ARRAY", true}, // text[] reports as ARRAY
		{"subject_pattern", "text", false},
		{"algorithms", "ARRAY", true},
		{"required_claims", "jsonb", true},
		{"created_at", "timestamp with time zone", true},
		{"updated_at", "timestamp with time zone", true},
		{"audit_login", "text", true},
	}
	for _, c := range wantCols {
		var dataType string
		var nullable string
		if err := pool.QueryRow(ctx, `
			select data_type, is_nullable from information_schema.columns
			 where table_name = 'oidc_trust_policies' and column_name = $1
		`, c.name).Scan(&dataType, &nullable); err != nil {
			t.Fatalf("column lookup %s: %v", c.name, err)
		}
		if dataType != c.typ {
			t.Errorf("column %s: want type %s, got %s", c.name, c.typ, dataType)
		}
		if c.notnull && nullable != "NO" {
			t.Errorf("column %s: want NOT NULL, got is_nullable=%s", c.name, nullable)
		}
	}

	// (2) Composite PK is (account_id, issuer_url). The
	// information_schema assertion walks the
	// key_column_usage + table_constraints views.
	var pkCount int
	if err := pool.QueryRow(ctx, `
		select count(*) from information_schema.table_constraints
		 where table_name = 'oidc_trust_policies'
		   and constraint_type = 'PRIMARY KEY'
	`).Scan(&pkCount); err != nil {
		t.Fatalf("pk lookup: %v", err)
	}
	if pkCount != 1 {
		t.Errorf("expected 1 PK on oidc_trust_policies, got %d", pkCount)
	}
	type pkCol struct {
		columnName string
		ordinal    int
	}
	var pkCols []pkCol
	rows, err := pool.Query(ctx, `
		select kcu.column_name, kcu.ordinal_position
		  from information_schema.table_constraints tc
		  join information_schema.key_column_usage kcu
		    on tc.constraint_name = kcu.constraint_name
		   and tc.table_name = kcu.table_name
		 where tc.table_name = 'oidc_trust_policies'
		   and tc.constraint_type = 'PRIMARY KEY'
		 order by kcu.ordinal_position
	`)
	if err != nil {
		t.Fatalf("pk cols lookup: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c pkCol
		if err := rows.Scan(&c.columnName, &c.ordinal); err != nil {
			t.Fatalf("pk cols scan: %v", err)
		}
		pkCols = append(pkCols, c)
	}
	if len(pkCols) != 2 {
		t.Fatalf("expected 2 PK columns, got %d (%+v)", len(pkCols), pkCols)
	}
	if pkCols[0].columnName != "account_id" || pkCols[1].columnName != "issuer_url" {
		t.Errorf("PK columns: want (account_id, issuer_url), got (%s, %s)",
			pkCols[0].columnName, pkCols[1].columnName)
	}

	// (3) Seed an account + insert a real policy row. We need a
	// real account to satisfy the FK constraint.
	const seedAcct = "00000000-0000-0000-0000-000000000269"
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan) values ($1::uuid, 'm265@example.com', 'free')
		on conflict (id) do nothing
	`, seedAcct); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into oidc_trust_policies
		    (account_id, issuer_url, jwks_url, audience, algorithms,
		     required_claims, audit_login)
		values ($1::uuid, 'https://token.actions.githubusercontent.com',
		        'https://token.actions.githubusercontent.com/.well-known/jwks',
		        ARRAY['faas.example.com']::text[],
		        ARRAY['RS256']::text[],
		        '{}'::jsonb,
		        'auto')
	`, seedAcct); err != nil {
		t.Fatalf("insert policy row: %v", err)
	}

	// (4) FK rejects an unknown account_id.
	_, err = pool.Exec(ctx, `
		insert into oidc_trust_policies
		    (account_id, issuer_url, jwks_url, audience, algorithms,
		     required_claims, audit_login)
		values ('99999999-9999-9999-9999-999999999999'::uuid, 'https://unknown.example',
		        'https://unknown.example/jwks',
		        ARRAY[]::text[],
		        ARRAY['RS256']::text[],
		        '{}'::jsonb,
		        'auto')
	`)
	if err == nil {
		t.Errorf("expected FK violation on unknown account_id; got nil")
	} else if !strings.Contains(err.Error(), "23503") {
		t.Errorf("expected 23503 FK violation, got %v", err)
	}

	// (5) ON DELETE CASCADE — deleting the account removes the row.
	if _, err := pool.Exec(ctx, `delete from accounts where id = $1`, seedAcct); err != nil {
		t.Fatalf("delete account: %v", err)
	}
	var rowCount int
	if err := pool.QueryRow(ctx, `
		select count(*) from oidc_trust_policies where account_id = $1
	`, seedAcct).Scan(&rowCount); err != nil {
		t.Fatalf("re-read after cascade: %v", err)
	}
	if rowCount != 0 {
		t.Errorf("expected 0 policy rows after account delete (CASCADE), got %d", rowCount)
	}

	// (6) Re-applying the migration is a no-op. apply_walk_test.go's
	// MAX(version_id) assertion already enforces the apply-order
	// invariant; this is just a sanity tripwire for IF NOT EXISTS.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Errorf("second MigrateUp: %v (expected nil for IF NOT EXISTS)", err)
	}
}
