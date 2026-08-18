//go:build !no_pg

// Migration-apply test for 00270 (oidc_exchanged_tokens). Pins
// the load-bearing PR-A schema contract:
//
//   1. The migration applies cleanly through 00270.
//   2. All 9 columns are present with the right types and NOT NULL
//      constraints.
//   3. The token_hash UNIQUE index is present and enforced.
//   4. The expires_at b-tree index is present.
//   5. The FK to accounts(id) accepts a real account and rejects
//      a foreign one (23503).
//   6. ON DELETE CASCADE: deleting the owning account removes the
//      exchanged-token row (PR-A's GDPR §17 G2 path).
//   7. Re-applying the migration is a no-op (IF NOT EXISTS).
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

func TestMigrations_00270_OIDCExchangedTokens(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}

	// (1) All 9 columns present with expected types. Postgres
	// reports timestamptz as "timestamp with time zone" in
	// information_schema; the "timestamptz" alias is a parser
	// shortcut, not the storage type. Same caveat as
	// pg_get_constraintdef (MEMORY.md/pg-get-constraintdef-shapes).
	type colSpec struct {
		name    string
		typ     string
		notnull bool
	}
	wantCols := []colSpec{
		{"id", "uuid", true},
		{"account_id", "uuid", true},
		{"token_hash", "bytea", true},
		{"expires_at", "timestamp with time zone", true},
		{"issuer_url", "text", true},
		{"subject", "text", true},
		{"audience", "ARRAY", true},
		{"jti", "text", false},
		{"created_at", "timestamp with time zone", true},
	}
	for _, c := range wantCols {
		var dataType string
		var nullable string
		if err := pool.QueryRow(ctx, `
			select data_type, is_nullable from information_schema.columns
			 where table_name = 'oidc_exchanged_tokens' and column_name = $1
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

	// (2) Indices present (UNIQUE on token_hash + b-tree on expires_at).
	wantIdx := []string{
		"oidc_exchanged_tokens_token_hash_key", // UNIQUE on token_hash
		"oidc_exchanged_tokens_expires_at_idx",
	}
	for _, idx := range wantIdx {
		var found bool
		if err := pool.QueryRow(ctx, `
			select exists(select 1 from pg_indexes
			               where tablename = 'oidc_exchanged_tokens' and indexname = $1)`, idx).Scan(&found); err != nil {
			t.Fatalf("index lookup %s: %v", idx, err)
		}
		if !found {
			t.Errorf("missing index oidc_exchanged_tokens.%s", idx)
		}
	}

	// (3) Seed an account + insert a real token row. We need a
	// real account to satisfy the FK constraint.
	const seedAcct = "00000000-0000-0000-0000-000000000270"
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan) values ($1::uuid, 'm266@example.com', 'free')
		on conflict (id) do nothing
	`, seedAcct); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	const seedID = "00000000-0000-0000-0000-000000000a66"
	if _, err := pool.Exec(ctx, `
		insert into oidc_exchanged_tokens
		    (id, account_id, token_hash, expires_at, issuer_url, subject,
		     audience, jti)
		values ($1::uuid, $2::uuid, '\xdeadbeef'::bytea,
		        now() + interval '5 minutes',
		        'https://token.actions.githubusercontent.com',
		        'repo:octocat/hello-world:ref:refs/heads/main',
		        ARRAY['faas.example.com']::text[],
		        'jt-12345')
	`, seedID, seedAcct); err != nil {
		t.Fatalf("insert token row: %v", err)
	}

	// (4) UNIQUE on token_hash is enforced. Insert a row with
	// the same hash; expect 23505 (unique violation).
	_, err := pool.Exec(ctx, `
		insert into oidc_exchanged_tokens
		    (id, account_id, token_hash, expires_at, issuer_url, subject,
		     audience)
		values ('00000000-0000-0000-0000-000000000b66'::uuid, $1::uuid,
		        '\xdeadbeef'::bytea,
		        now() + interval '5 minutes',
		        'https://token.actions.githubusercontent.com',
		        'sub-2', ARRAY[]::text[])
	`, seedAcct)
	if err == nil {
		t.Errorf("expected UNIQUE violation on duplicate token_hash; got nil")
	} else if !strings.Contains(err.Error(), "23505") {
		t.Errorf("expected 23505 UNIQUE violation, got %v", err)
	}

	// (5) FK rejects an unknown account_id.
	_, err = pool.Exec(ctx, `
		insert into oidc_exchanged_tokens
		    (id, account_id, token_hash, expires_at, issuer_url, subject,
		     audience)
		values ('00000000-0000-0000-0000-000000000c66'::uuid,
		        '99999999-9999-9999-9999-999999999999'::uuid,
		        '\xcafe0000'::bytea,
		        now() + interval '5 minutes',
		        'https://unknown.example',
		        'sub-3', ARRAY[]::text[])
	`)
	if err == nil {
		t.Errorf("expected FK violation on unknown account_id; got nil")
	} else if !strings.Contains(err.Error(), "23503") {
		t.Errorf("expected 23503 FK violation, got %v", err)
	}

	// (6) ON DELETE CASCADE — deleting the account removes the
	// token row.
	if _, err := pool.Exec(ctx, `delete from accounts where id = $1`, seedAcct); err != nil {
		t.Fatalf("delete account: %v", err)
	}
	var rowCount int
	if err := pool.QueryRow(ctx, `
		select count(*) from oidc_exchanged_tokens where account_id = $1
	`, seedAcct).Scan(&rowCount); err != nil {
		t.Fatalf("re-read after cascade: %v", err)
	}
	if rowCount != 0 {
		t.Errorf("expected 0 token rows after account delete (CASCADE), got %d", rowCount)
	}

	// (7) Re-applying the migration is a no-op. apply_walk_test.go's
	// MAX(version_id) assertion already enforces the apply-order
	// invariant; this is just a sanity tripwire for IF NOT EXISTS.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Errorf("second MigrateUp: %v (expected nil for IF NOT EXISTS)", err)
	}
}
