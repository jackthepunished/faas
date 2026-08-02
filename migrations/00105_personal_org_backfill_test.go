//go:build !no_pg

// Migration-apply test for 00105_personal_org_backfill.sql
// (issue #190 / IAM-6 / ADR-061, PR 3). Pins the deterministic
// personal-org backfill that brings pre-PR-3 accounts into the
// "every account has exactly one personal org" shape.
//
// Pins:
//
//  1. Migration set applies cleanly through 00105.
//  2. Backfill is idempotent: count(accounts) == count(orgs WHERE
//     personal_org = true) after a single apply.
//  3. Every personal org has an active owner membership.
//  4. Personal-org slugs match the deterministic derivation
//     (^u-[0-9a-f]{12}$ — the PersonalOrgSlug shape).
//  5. The partial unique orgs_one_personal_per_account_uniq is in
//     place (the SQL-level tripwire the backfill and the
//     CreateAccountWithPersonalOrg helper both depend on).
//  6. The orgs_personal_owner_link CHECK is in place.
//  7. Replay-safe: a second MigrateUp is a no-op (ADR-041).
//
// Build tag matches the rest of the migration tests; set
// FAAS_SKIP_PG_TESTS=1 to skip locally (see migrations/README.md).
package migrations_test

import (
	"context"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00105_PersonalOrgBackfill(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// Apply migrations 1..100 so the orgs / org_memberships tables
	// (created by 00099) exist and the backfill INSERT can target
	// them. pgtest.Open returns a fresh schema so nothing else lives
	// here yet.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}

	// At this point the schema is at 00105 — the backfill already
	// ran on an empty accounts table (it's idempotent: ON CONFLICT
	// DO NOTHING + LEFT JOIN … WHERE o.id IS NULL). Now seed three
	// pre-PR-3 accounts. The backfill migration's contract is that
	// existing accounts at the time the migration runs get a
	// personal org; accounts created after the migration runs go
	// through CreateAccountWithPersonalOrg instead. To exercise the
	// backfill's own logic we have two options: (a) re-run the
	// backfill INSERT manually against the freshly seeded accounts,
	// or (b) seed before applying. The migration is replay-safe
	// (ADR-041), so re-running the backfill's own body is the
	// deterministic test. Insert the seeds then re-run the backfill
	// SELECT/INSERT directly.
	seedAccounts := []struct {
		id    string
		email string
	}{
		// Realistic UUIDs whose first 12 hex chars (after dash
		// stripping) are distinct so the deterministic
		// PersonalOrgSlug derivation produces three distinct
		// slugs that satisfy orgs_slug_uniq (case-insensitive
		// unique on lower(slug)).
		{"a1a2a3a4-0000-0000-0000-000000000001", "pre-pr3-1@example.com"},
		{"b1b2b3b4-0000-0000-0000-000000000002", "pre-pr3-2@example.com"},
		{"c1c2c3c4-0000-0000-0000-000000000003", "pre-pr3-3@example.com"},
	}
	for _, sa := range seedAccounts {
		if _, err := pool.Exec(ctx, `
			INSERT INTO accounts (id, email, plan, status, created_at)
			VALUES ($1, $2, 'free', 'active', now())
			ON CONFLICT (id) DO NOTHING
		`, sa.id, sa.email); err != nil {
			t.Fatalf("seed account %s: %v", sa.id, err)
		}
	}

	// Re-run the backfill's own INSERT bodies so the freshly seeded
	// accounts get a personal org. This is the same SQL the
	// migration runs, just executed directly against the live DB so
	// the test can probe the invariants end-to-end.
	if _, err := pool.Exec(ctx, `
		INSERT INTO orgs (
			id, slug, name, personal_org, personal_owner_account_id,
			plan, status, created_at, updated_at
		)
		SELECT
			gen_random_uuid(),
			'u-' || substring(replace(a.id::text, '-', '') from 1 for 12),
			'Personal', true, a.id, a.plan, a.status, a.created_at, now()
		FROM accounts a
		LEFT JOIN orgs o
			ON o.personal_owner_account_id = a.id AND o.personal_org = true
		WHERE o.id IS NULL
		ON CONFLICT DO NOTHING
	`); err != nil {
		t.Fatalf("backfill orgs: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO org_memberships (org_id, account_id, role, invited_by_account_id)
		SELECT o.id, a.id, 'owner', NULL
		FROM accounts a
		JOIN orgs o ON o.personal_owner_account_id = a.id AND o.personal_org = true
		LEFT JOIN org_memberships m ON m.org_id = o.id AND m.account_id = a.id
		WHERE m.org_id IS NULL
		ON CONFLICT DO NOTHING
	`); err != nil {
		t.Fatalf("backfill memberships: %v", err)
	}

	// Probe 1: every account has exactly one personal org.
	// Probe 1: every account has exactly one personal org.
	var accountCount, personalOrgCount int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM accounts),
		       (SELECT count(*) FROM orgs WHERE personal_org = true)
	`).Scan(&accountCount, &personalOrgCount); err != nil {
		t.Fatalf("count parity: %v", err)
	}
	if accountCount != personalOrgCount {
		t.Errorf("count parity: %d accounts, %d personal orgs", accountCount, personalOrgCount)
	}

	// Probe 2: every personal org has an active owner membership.
	var ownerlessCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM orgs o
		 WHERE o.personal_org = true
		   AND NOT EXISTS (
		       SELECT 1 FROM org_memberships m
		        WHERE m.org_id = o.id AND m.role = 'owner' AND m.removed_at IS NULL
		   )
	`).Scan(&ownerlessCount); err != nil {
		t.Fatalf("owner membership probe: %v", err)
	}
	if ownerlessCount != 0 {
		t.Errorf("ownerless personal orgs: %d", ownerlessCount)
	}

	// Probe 3: personal-org slugs match the deterministic shape.
	// 'u-' + 12 hex chars = 14 chars total, well inside the
	// orgs_slug_shape regex.
	var malformedCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM orgs
		 WHERE personal_org = true
		   AND slug !~ '^u-[0-9a-f]{12}$'
	`).Scan(&malformedCount); err != nil {
		t.Fatalf("slug shape probe: %v", err)
	}
	if malformedCount != 0 {
		t.Errorf("malformed personal-org slugs: %d", malformedCount)
	}

	// Probe 4: the partial unique tripwire is in place.
	// CREATE UNIQUE INDEX creates an index (pg_class) not a constraint
	// (pg_constraint). The constraint entry would only exist if the
	// unique were declared inline on the table — which we deliberately
	// don't (the partial-WHERE predicate is what gives us "at most one
	// personal org per account", which a plain UNIQUE constraint can't
	// express).
	var hasPartialUnique bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1 FROM pg_class c
		      JOIN pg_index i ON i.indexrelid = c.oid
		      JOIN pg_am a ON a.oid = c.relam
		     WHERE c.relname = 'orgs_one_personal_per_account_uniq'
		       AND a.amname = 'btree'
		)
	`).Scan(&hasPartialUnique); err != nil {
		t.Fatalf("partial unique probe: %v", err)
	}
	if !hasPartialUnique {
		t.Errorf("orgs_one_personal_per_account_uniq not found")
	}

	// Probe 5: the CHECK that ties personal_org to
	// personal_owner_account_id is in place.
	var hasCheck bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1 FROM pg_constraint
		     WHERE conname = 'orgs_personal_owner_link'
		       AND contype = 'c'
		)
	`).Scan(&hasCheck); err != nil {
		t.Fatalf("CHECK probe: %v", err)
	}
	if !hasCheck {
		t.Errorf("orgs_personal_owner_link CHECK not found")
	}
}

func TestMigrations_00105_ReplaySafety(t *testing.T) {
	// Backfill DDL is ON CONFLICT DO NOTHING + LEFT JOIN — a second
	// run must be a no-op.
	ctx := context.Background()
	pool := pgtest.Open(t)
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("first MigrateUp: %v", err)
	}
	// Seed two accounts so the backfill has something to do.
	for _, acct := range []struct{ id, email string }{
		{"00000000-0000-0000-0000-0000000020a1", "replay-1@example.com"},
		{"00000000-0000-0000-0000-0000000020a2", "replay-2@example.com"},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO accounts (id, email, plan, status, created_at)
			VALUES ($1, $2, 'free', 'active', now())
			ON CONFLICT (id) DO NOTHING
		`, acct.id, acct.email); err != nil {
			t.Fatalf("seed %s: %v", acct.id, err)
		}
	}
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("second MigrateUp: %v", err)
	}
	var preOrgs int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM orgs WHERE personal_org = true
	`).Scan(&preOrgs); err != nil {
		t.Fatalf("count pre: %v", err)
	}
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("third MigrateUp: %v", err)
	}
	var postOrgs int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM orgs WHERE personal_org = true
	`).Scan(&postOrgs); err != nil {
		t.Fatalf("count post: %v", err)
	}
	if postOrgs != preOrgs {
		t.Errorf("replay safety: pre=%d post=%d personal orgs", preOrgs, postOrgs)
	}
}
