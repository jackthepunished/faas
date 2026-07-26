//go:build !no_pg

// Migration-apply test for 00050 (github_install_account_id +
// github_install_binding_id + github_install_linked_at). Pins the
// load-bearing PR-B schema contract:
//
//   1. The migration applies cleanly through 00050.
//   2. The three new columns are present on `apps`.
//   3. The account-FK accepts NULL (apps without GitHub bindings stay
//      valid), accepts a real account_id, and rejects an unknown
//      account_id with a FK violation.
//   4. The account-scoped unique partial index
//      (apps_github_install_account_uniq) rejects duplicate
//      (account_id, binding_id) tuples but allows the same binding_id
//      under a different account.
//   5. The (repo, branch) lookup index
//      (apps_github_install_repo_branch_idx) is present.
//   6. ON DELETE SET NULL: deleting the account clears
//      github_install_account_id but leaves the app row intact.
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

// TestMigrations_00050_GithubBindingsAccount pins the schema contract
// for the per-account GitHub install binding columns added by PR-B.
func TestMigrations_00050_GithubBindingsAccount(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}

	// (1) Columns present.
	wantCols := []string{
		"github_install_binding_id",
		"github_install_account_id",
		"github_install_linked_at",
	}
	for _, col := range wantCols {
		var found bool
		if err := pool.QueryRow(ctx, `
			select exists(select 1 from information_schema.columns
			               where table_name = 'apps' and column_name = $1)`, col).Scan(&found); err != nil {
			t.Fatalf("column lookup %s: %v", col, err)
		}
		if !found {
			t.Errorf("missing column apps.%s", col)
		}
	}

	// (2) Indices present.
	wantIdx := []string{
		"apps_github_install_account_uniq",
		"apps_github_install_account_idx",
		"apps_github_install_repo_branch_idx",
	}
	for _, idx := range wantIdx {
		var found bool
		if err := pool.QueryRow(ctx, `
			select exists(select 1 from pg_indexes
			               where tablename = 'apps' and indexname = $1)`, idx).Scan(&found); err != nil {
			t.Fatalf("index lookup %s: %v", idx, err)
		}
		if !found {
			t.Errorf("missing index apps.%s", idx)
		}
	}

	// (3) NULL account_id is accepted (apps without bindings stay
	// valid); the new FK does not impose a non-null requirement.
	if _, err := pool.Exec(ctx, `
		insert into apps (slug, account_id, ram_mb)
		values ('m050-no-bind-1', '00000000-0000-0000-0000-000000000050'::uuid, 128)
	`); err != nil {
		// Most likely the account row is missing; seed and retry.
		if _, serr := pool.Exec(ctx, `
			insert into accounts (id, email, plan)
			values ('00000000-0000-0000-0000-000000000050'::uuid, 'm050@example.com', 'free')
			on conflict do nothing
		`); serr != nil {
			t.Fatalf("seed account: %v", serr)
		}
		if _, err := pool.Exec(ctx, `
			insert into apps (slug, account_id, ram_mb)
			values ('m050-no-bind-1', '00000000-0000-0000-0000-000000000050'::uuid, 128)
		`); err != nil {
			t.Fatalf("insert app with no binding: %v", err)
		}
	}

	// (4) FK rejects an unknown account_id. We need an existing app
	// to UPDATE into a bad value; create one without a binding first.
	const seedAcct = "00000000-0000-0000-0000-000000000501"
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan) values
		  ($1::uuid, 'm050-fk@example.com', 'hobby')
		on conflict (id) do nothing
	`, seedAcct); err != nil {
		t.Fatalf("seed fk account: %v", err)
	}
	var seedAppID string
	if err := pool.QueryRow(ctx, `
		insert into apps (slug, account_id, ram_mb)
		values ('m050-fk-target', $1::uuid, 128)
		returning id
	`, seedAcct).Scan(&seedAppID); err != nil {
		t.Fatalf("seed fk app: %v", err)
	}
	_, err := pool.Exec(ctx, `
		update apps set github_install_account_id = '99999999-9999-9999-9999-999999999999'::uuid
		 where id = $1
	`, seedAppID)
	if err == nil {
		t.Errorf("expected FK violation on unknown account_id; got nil")
	} else if !strings.Contains(err.Error(), "23503") {
		// 23503 = foreign_key_violation. We don't pin the message
		// string — it varies by Postgres version.
		t.Errorf("expected 23503 FK violation, got %v", err)
	}

	// (5) Account-scoped unique partial index rejects duplicate
	// (account_id, binding_id) but allows the same binding_id under a
	// different account.
	const acctA = "00000000-0000-0000-0000-000000000a50"
	const acctB = "00000000-0000-0000-0000-000000000b50"
	for _, a := range []string{acctA, acctB} {
		if _, err := pool.Exec(ctx, `
			insert into accounts (id, email, plan) values ($1::uuid, $2, 'hobby')
			on conflict (id) do nothing
		`, a, "m050-unq-"+a[len(a)-3:]+"@example.com"); err != nil {
			t.Fatalf("seed unq account %s: %v", a, err)
		}
	}
	var unqApp1 string
	if err := pool.QueryRow(ctx, `
		insert into apps (slug, account_id, ram_mb, github_install_account_id, github_install_binding_id)
		values ('m050-unq-1', $1::uuid, 128, $1::uuid, 'bind-shared')
		returning id
	`, acctA).Scan(&unqApp1); err != nil {
		t.Fatalf("insert first binding: %v", err)
	}
	_, err = pool.Exec(ctx, `
		insert into apps (slug, account_id, ram_mb, github_install_account_id, github_install_binding_id)
		values ('m050-unq-2', $1::uuid, 128, $1::uuid, 'bind-shared')
	`, acctA)
	if err == nil {
		t.Errorf("expected unique violation on duplicate (account_id, binding_id); got nil")
	} else if !strings.Contains(err.Error(), "apps_github_install_account_uniq") {
		// Postgres 16 reports the index name in the error; we pin
		// on the index name to keep the assertion stable.
		t.Errorf("expected violation on apps_github_install_account_uniq, got %v", err)
	}
	// Same binding_id under a different account: allowed.
	var unqApp3 string
	if err := pool.QueryRow(ctx, `
		insert into apps (slug, account_id, ram_mb, github_install_account_id, github_install_binding_id)
		values ('m050-unq-3', $1::uuid, 128, $1::uuid, 'bind-shared')
		returning id
	`, acctB).Scan(&unqApp3); err != nil {
		t.Errorf("expected (bind-shared, acctB) to be allowed; got %v", err)
	}
	if unqApp1 == "" || unqApp3 == "" || unqApp1 == unqApp3 {
		t.Errorf("expected distinct app ids, got %q and %q", unqApp1, unqApp3)
	}

	// (6) ON DELETE SET NULL — deleting the account clears
	// github_install_account_id but leaves the app row. The app's
	// owner (apps.account_id) MUST be a different account from the
	// one referenced via github_install_account_id — otherwise the
	// apps_account_id_fkey RESTRICT blocks the delete before the
	// SET NULL trigger on github_install_account_id can fire.
	// Real-world shape: account A owns the app; account B (the
	// deleted GitHub user) is the install owner; B's deletion
	// nulls the install reference, not the app row.
	const ownerAcct = "00000000-0000-0000-0000-000000000e50"
	const delAcct = "00000000-0000-0000-0000-000000000d50"
	for _, a := range []string{ownerAcct, delAcct} {
		if _, err := pool.Exec(ctx, `
			insert into accounts (id, email, plan) values ($1::uuid, $2, 'hobby')
			on conflict (id) do nothing
		`, a, "m050-del-"+a[len(a)-3:]+"@example.com"); err != nil {
			t.Fatalf("seed del account %s: %v", a, err)
		}
	}
	var delAppID string
	if err := pool.QueryRow(ctx, `
		insert into apps (slug, account_id, ram_mb, github_install_account_id, github_install_binding_id)
		values ('m050-del-1', $1::uuid, 128, $2::uuid, 'bind-del')
		returning id
	`, ownerAcct, delAcct).Scan(&delAppID); err != nil {
		t.Fatalf("seed del app: %v", err)
	}
	if _, err := pool.Exec(ctx, `delete from accounts where id = $1`, delAcct); err != nil {
		t.Fatalf("delete account: %v", err)
	}
	var accountID *string
	if err := pool.QueryRow(ctx, `
		select github_install_account_id from apps where id = $1
	`, delAppID).Scan(&accountID); err != nil {
		t.Fatalf("re-read app: %v", err)
	}
	if accountID != nil {
		t.Errorf("expected github_install_account_id NULL after account delete, got %v", *accountID)
	}
}
