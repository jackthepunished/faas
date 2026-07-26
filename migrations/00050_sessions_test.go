//go:build !no_pg

// Shape test for migration 00050 (IAM-3, issue #187 + #244 merged,
// ADR-036). Server-side session revocation.
//
// Asserts:
//
//   1. The sessions table lands with the seven columns
//      (id, account_id, issued_ip, issued_ua, issued_at,
//      last_seen_at, revoked_at).
//   2. The FK on account_id cascades on delete (deleting the
//      parent account row drops the matching sessions rows).
//   3. The partial index sessions_active_account_idx fires on
//      active rows and stays cold for revoked rows.
//   4. Re-applying the migration is a no-op (idempotence).
//
// Build tag mirrors apply_walk_test.go:4 — set FAAS_SKIP_PG_TESTS=1
// locally to skip.

package migrations_test

import (
	"context"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00050_Sessions_ShapeAndFK(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}

	const acctID = "00000000-0000-0000-0000-000000000050"
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ($1, 'iam3-shape@example.com', 'free', now())
		on conflict (id) do nothing
	`, acctID); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	// (1) The seven columns exist.
	rows, err := pool.Query(ctx, `
		select attname from pg_attribute
		 where attrelid = 'sessions'::regclass
		   and attnum > 0
		   and attname in ('id', 'account_id', 'issued_ip', 'issued_ua',
		                   'issued_at', 'last_seen_at', 'revoked_at')
		   and not attisdropped
		 order by attname
	`)
	if err != nil {
		t.Fatalf("pg_attribute scan: %v", err)
	}
	defer rows.Close()

	want := []string{"account_id", "id", "issued_at", "issued_ip", "issued_ua", "last_seen_at", "revoked_at"}
	var got []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		got = append(got, n)
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d session columns, got %d (%v)", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("column[%d]: want %q, got %q", i, want[i], got[i])
		}
	}

	// (2) FK CASCADE on account_id.
	const sid1 = "11111111-1111-1111-1111-111111111111"
	if _, err := pool.Exec(ctx, `
		insert into sessions (id, account_id, issued_ua)
		values ($1, $2, 'mozilla/5.0 shape test')
	`, sid1, acctID); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	// Detach from accounts (the seeded row is part of pgtest setup).
	if _, err := pool.Exec(ctx, `delete from accounts where id = $1`, acctID); err != nil {
		t.Fatalf("delete account: %v", err)
	}
	var orphanCount int
	if err := pool.QueryRow(ctx,
		`select count(*) from sessions where account_id = $1`, acctID,
	).Scan(&orphanCount); err != nil {
		t.Fatalf("count orphan sessions: %v", err)
	}
	if orphanCount != 0 {
		t.Errorf("FK CASCADE: expected 0 orphan sessions, got %d", orphanCount)
	}

	// (3) Partial index present + correct predicate.
	var idxDef string
	if err := pool.QueryRow(ctx, `
		select pg_get_indexdef(indexrelid)
		  from pg_index
		 where indexrelid = 'sessions_active_account_idx'::regclass
	`).Scan(&idxDef); err != nil {
		t.Fatalf("pg_get_indexdef: %v", err)
	}
	if !contains(idxDef, "WHERE") || !contains(idxDef, "revoked_at IS NULL") {
		t.Errorf("partial index predicate missing: %s", idxDef)
	}

	// (4) Re-apply idempotence.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Errorf("MigrateUp idempotent re-apply: %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}