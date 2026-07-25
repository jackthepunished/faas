//go:build !no_pg

// Migration-apply test for 00036 (oauth_links). Pins the load-bearing §11
// anti-takeover contract from ADR-032 / issue #165 PR #2:
//
//   1. The migration set applies cleanly through 00036.
//   2. The table exposes the documented columns + the composite PK on
//      (provider, provider_subject) is the load-bearing anti-takeover
//      invariant. A duplicate (provider, sub) on a DIFFERENT account_id
//      must fail — that's the closure: once an OAuth subject binds, the
//      first party to bind it owns the row, period.
//   3. ON DELETE CASCADE on the (account_id) FK drops the link when the
//      account is hard-deleted (GDPR G6 / ADR-021 path).
//   4. The (account_id) index exists so the future "list providers bound
//      to this account" dashboard hint doesn't full-table scan.
//
// Build tag mirrors 00030_invocations_test.go:21.

package migrations_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

// TestMigrations_00036_OAuthLinks pins the schema + PK contract for the
// OAuth-links table. Mirrors the 00029/00030 shape: one test, comprehensive
// coverage, no per-feature drift.
func TestMigrations_00036_OAuthLinks(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}

	t.Run("columns_and_index", func(t *testing.T) {
		wantCols := []string{"provider", "provider_subject", "account_id", "email", "email_verified", "created_at"}
		for _, col := range wantCols {
			var found bool
			if err := pool.QueryRow(ctx, `
				select exists(select 1 from information_schema.columns
				               where table_name = 'oauth_links' and column_name = $1)`, col).Scan(&found); err != nil {
				t.Fatalf("column lookup %s: %v", col, err)
			}
			if !found {
				t.Errorf("missing column oauth_links.%s", col)
			}
		}

		// The (account_id) index supports the future "list providers bound
		// to this account" dashboard hint. The PK on (provider, subject)
		// already covers the OAuth callback hot path; this index is for
		// the reverse direction.
		var idxFound bool
		if err := pool.QueryRow(ctx, `
			select exists(select 1 from pg_indexes
			               where tablename = 'oauth_links' and indexname = 'oauth_links_account_idx')`).Scan(&idxFound); err != nil {
			t.Fatalf("index lookup oauth_links_account_idx: %v", err)
		}
		if !idxFound {
			t.Errorf("missing index oauth_links_account_idx")
		}
	})

	t.Run("pk_rejects_duplicate_subject_on_different_account", func(t *testing.T) {
		// Seed two accounts. A unique (provider, subject) on the FIRST
		// account must reject a re-insert of the SAME (provider, subject)
		// on the SECOND account — that's the §11 invariant in database
		// form. Without the PK, a hand-rolled apid bug (or a SQL tool
		// that bypasses UpsertOAuthLink) could let two accounts share an
		// OAuth subject, which is exactly the takeover scenario.
		if _, err := pool.Exec(ctx, `
			insert into accounts (id, email, plan, created_at)
			values ('00000000-0000-0000-0000-000000000033'::uuid, 'oauth-link-a@localhost', 'free', now())
			on conflict (id) do nothing
		`); err != nil {
			t.Fatalf("seed account A: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			insert into accounts (id, email, plan, created_at)
			values ('00000000-0000-0000-0000-000000000133'::uuid, 'oauth-link-b@localhost', 'free', now())
			on conflict (id) do nothing
		`); err != nil {
			t.Fatalf("seed account B: %v", err)
		}

		// First insert: account A binds (google, sub-1).
		if _, err := pool.Exec(ctx, `
			insert into oauth_links (provider, provider_subject, account_id, email, email_verified)
			values ('google', 'sub-1', '00000000-0000-0000-0000-000000000033'::uuid, 'oauth-link-a@localhost', true)
		`); err != nil {
			t.Fatalf("first oauth_link insert: %v", err)
		}

		// Second insert: SAME (google, sub-1) on account B must fail.
		_, err := pool.Exec(ctx, `
			insert into oauth_links (provider, provider_subject, account_id, email, email_verified)
			values ('google', 'sub-1', '00000000-0000-0000-0000-000000000133'::uuid, 'oauth-link-b@localhost', true)
		`)
		if err == nil {
			t.Fatalf("duplicate (provider, subject) on a different account was accepted; PK did not enforce the §11 invariant")
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("duplicate-subject error not a *pgconn.PgError: %v", err)
		}
		if pgErr.Code != "23505" {
			t.Errorf("duplicate-subject SQLSTATE = %q, want %q (unique_violation); full: %v", pgErr.Code, "23505", err)
		}
	})

	t.Run("cascade_deletes_with_account", func(t *testing.T) {
		// Seed a fresh account + link. Deleting the account should drop
		// the link (ON DELETE CASCADE) so the GDPR G6 hard-delete path
		// leaves no orphaned identity binding behind.
		accountID := "00000000-0000-0000-0000-000000000233"
		if _, err := pool.Exec(ctx, `
			insert into accounts (id, email, plan, created_at)
			values ($1::uuid, 'oauth-link-c@localhost', 'free', now())
			on conflict (id) do nothing
		`, accountID); err != nil {
			t.Fatalf("seed account C: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			insert into oauth_links (provider, provider_subject, account_id, email, email_verified)
			values ('github', 'gh-42', $1::uuid, 'oauth-link-c@localhost', true)
		`, accountID); err != nil {
			t.Fatalf("insert link on C: %v", err)
		}

		// Sanity: the link exists.
		var present bool
		if err := pool.QueryRow(ctx, `
			select exists(select 1 from oauth_links
			               where provider = 'github' and provider_subject = 'gh-42')
		`).Scan(&present); err != nil {
			t.Fatalf("link existence check: %v", err)
		}
		if !present {
			t.Fatalf("link not present before cascade test")
		}

		// Delete the account.
		if _, err := pool.Exec(ctx, `delete from accounts where id = $1::uuid`, accountID); err != nil {
			t.Fatalf("delete account C: %v", err)
		}

		// Link must be gone.
		if err := pool.QueryRow(ctx, `
			select exists(select 1 from oauth_links
			               where provider = 'github' and provider_subject = 'gh-42')
		`).Scan(&present); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("post-cascade link existence check: %v", err)
		}
		if present {
			t.Errorf("oauth_link survived account deletion; ON DELETE CASCADE did not fire")
		}
	})

	t.Run("different_provider_same_sub_does_not_collide", func(t *testing.T) {
		// Sanity: the PK is (provider, subject), so (google, foo) and
		// (github, foo) are distinct rows. This pins the "provider is
		// part of the identity" property — a future bug that drops
		// `provider` from the PK would surface here.
		accountID := "00000000-0000-0000-0000-000000000333"
		if _, err := pool.Exec(ctx, `
			insert into accounts (id, email, plan, created_at)
			values ($1::uuid, 'oauth-link-d@localhost', 'free', now())
			on conflict (id) do nothing
		`, accountID); err != nil {
			t.Fatalf("seed account D: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			insert into oauth_links (provider, provider_subject, account_id, email, email_verified)
			values ('google', 'shared-sub', $1::uuid, 'oauth-link-d@localhost', true)
		`, accountID); err != nil {
			t.Fatalf("insert google/shared-sub: %v", err)
		}
		// Same `sub` value, different provider — must NOT collide.
		if _, err := pool.Exec(ctx, `
			insert into oauth_links (provider, provider_subject, account_id, email, email_verified)
			values ('github', 'shared-sub', $1::uuid, 'oauth-link-d@localhost', true)
		`, accountID); err != nil {
			t.Errorf("(github, shared-sub) collided with (google, shared-sub): PK is not (provider, subject) composite — %v", err)
		}
	})
}
