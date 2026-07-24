//go:build !no_pg

// Migration-apply test for 00034 (account_passwords). Pins the load-bearing
// Argon2id-storage contract from ADR-032 / issue #165 PR #2:
//
//   1. The migration set applies cleanly through 00034.
//   2. The table exposes (account_id PK, hash text NOT NULL, updated_at
//      timestamptz NOT NULL DEFAULT now()).
//   3. The PK is on account_id — one row per account. A second insert
//      for the same account fails the PK (the apid code path upserts
//      instead; the PK protects against racing duplicate inserts).
//   4. ON DELETE CASCADE on the FK drops the row when the account is
//      hard-deleted (GDPR G6 / ADR-021 path).
//   5. hash is NOT NULL — a row written without a hash is rejected at
//      the database floor, not just the apid validator.
//
// Build tag mirrors 00030_invocations_test.go:21.

package migrations_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

// TestMigrations_00034_AccountPasswords pins the schema + PK + NOT NULL
// contract for the Argon2id hash storage. Mirrors the 00029/00033 shape.
func TestMigrations_00034_AccountPasswords(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}

	t.Run("columns_and_defaults", func(t *testing.T) {
		wantCols := []string{"account_id", "hash", "updated_at"}
		for _, col := range wantCols {
			var found bool
			if err := pool.QueryRow(ctx, `
				select exists(select 1 from information_schema.columns
				               where table_name = 'account_passwords' and column_name = $1)`, col).Scan(&found); err != nil {
				t.Fatalf("column lookup %s: %v", col, err)
			}
			if !found {
				t.Errorf("missing column account_passwords.%s", col)
			}
		}

		// updated_at must default to now(). A regression that drops the
		// default would surface here — the apid code path reads this for
		// the "rotate hash on login" PR #2.5 hardening.
		var hasDefault bool
		if err := pool.QueryRow(ctx, `
			select (column_default is not null)
			  from information_schema.columns
			 where table_name = 'account_passwords' and column_name = 'updated_at'
		`).Scan(&hasDefault); err != nil {
			t.Fatalf("updated_at default lookup: %v", err)
		}
		if !hasDefault {
			t.Errorf("account_passwords.updated_at has no DEFAULT; the column is supposed to default to now()")
		}
	})

	t.Run("one_row_per_account", func(t *testing.T) {
		// Seed an account. First password row inserts. A second insert
		// for the SAME account must fail the PK — protecting against
		// racing concurrent SetAccountPassword calls. (apid uses
		// INSERT ... ON CONFLICT DO UPDATE; the PK is the floor.)
		accountID := "00000000-0000-0000-0000-000000000034"
		if _, err := pool.Exec(ctx, `
			insert into accounts (id, email, plan, created_at)
			values ($1::uuid, 'pw-test-a@localhost', 'free', now())
			on conflict (id) do nothing
		`, accountID); err != nil {
			t.Fatalf("seed account: %v", err)
		}

		// First row inserts cleanly.
		if _, err := pool.Exec(ctx, `
			insert into account_passwords (account_id, hash)
			values ($1::uuid, '$argon2id$v=19$m=65536,t=1,p=2$AAAA$BBBB')
		`, accountID); err != nil {
			t.Fatalf("first password insert: %v", err)
		}

		// Second row on the same account fails the PK.
		_, err := pool.Exec(ctx, `
			insert into account_passwords (account_id, hash)
			values ($1::uuid, '$argon2id$v=19$m=65536,t=1,p=2$CCCC$DDDD')
		`, accountID)
		if err == nil {
			t.Fatalf("second password row for the same account was accepted; PK is not enforcing one-row-per-account")
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("duplicate-row error not a *pgconn.PgError: %v", err)
		}
		if pgErr.Code != "23505" {
			t.Errorf("duplicate-row SQLSTATE = %q, want %q (unique_violation); full: %v", pgErr.Code, "23505", err)
		}
	})

	t.Run("hash_not_null", func(t *testing.T) {
		// A row with hash = NULL must be rejected. This pins the
		// NOT NULL contract at the database floor — even if the apid
		// validator ever drifts, a hand-rolled INSERT with NULL hash
		// will fail here, not silently produce a row that AuthLogin
		// can't verify.
		accountID := "00000000-0000-0000-0000-000000000134"
		if _, err := pool.Exec(ctx, `
			insert into accounts (id, email, plan, created_at)
			values ($1::uuid, 'pw-test-b@localhost', 'free', now())
			on conflict (id) do nothing
		`, accountID); err != nil {
			t.Fatalf("seed account: %v", err)
		}

		_, err := pool.Exec(ctx, `
			insert into account_passwords (account_id, hash) values ($1::uuid, NULL)
		`, accountID)
		if err == nil {
			t.Errorf("insert with hash = NULL was accepted; NOT NULL did not fire")
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("null-hash error not a *pgconn.PgError: %v", err)
		}
		if pgErr.Code != "23502" {
			t.Errorf("null-hash SQLSTATE = %q, want %q (not_null_violation); full: %v", pgErr.Code, "23502", err)
		}
	})

	t.Run("cascade_deletes_with_account", func(t *testing.T) {
		// Deleting the account drops the password row. The G6 hard-delete
		// path doesn't have to remember to clear passwords separately.
		accountID := "00000000-0000-0000-0000-000000000234"
		if _, err := pool.Exec(ctx, `
			insert into accounts (id, email, plan, created_at)
			values ($1::uuid, 'pw-test-c@localhost', 'free', now())
			on conflict (id) do nothing
		`, accountID); err != nil {
			t.Fatalf("seed account: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			insert into account_passwords (account_id, hash)
			values ($1::uuid, '$argon2id$v=19$m=65536,t=1,p=2$EEEE$FFFF')
		`, accountID); err != nil {
			t.Fatalf("insert password: %v", err)
		}

		// Sanity: row exists.
		var present bool
		if err := pool.QueryRow(ctx, `
			select exists(select 1 from account_passwords where account_id = $1::uuid)
		`, accountID).Scan(&present); err != nil {
			t.Fatalf("password existence check: %v", err)
		}
		if !present {
			t.Fatalf("password row not present before cascade test")
		}

		// Delete the account.
		if _, err := pool.Exec(ctx, `delete from accounts where id = $1::uuid`, accountID); err != nil {
			t.Fatalf("delete account: %v", err)
		}

		// Password row must be gone.
		if err := pool.QueryRow(ctx, `
			select exists(select 1 from account_passwords where account_id = $1::uuid)
		`, accountID).Scan(&present); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("post-cascade existence check: %v", err)
		}
		if present {
			t.Errorf("account_passwords row survived account deletion; ON DELETE CASCADE did not fire")
		}
	})

	t.Run("updated_at_stamps_on_insert", func(t *testing.T) {
		// The column defaults to now() at INSERT; verify it lands within
		// a sane window so a regression that drops the default is caught.
		accountID := "00000000-0000-0000-0000-000000000334"
		if _, err := pool.Exec(ctx, `
			insert into accounts (id, email, plan, created_at)
			values ($1::uuid, 'pw-test-d@localhost', 'free', now())
			on conflict (id) do nothing
		`, accountID); err != nil {
			t.Fatalf("seed account: %v", err)
		}

		before := time.Now().Add(-1 * time.Second)
		if _, err := pool.Exec(ctx, `
			insert into account_passwords (account_id, hash)
			values ($1::uuid, '$argon2id$v=19$m=65536,t=1,p=2$GGGG$HHHH')
		`, accountID); err != nil {
			t.Fatalf("insert password: %v", err)
		}

		var stamped time.Time
		if err := pool.QueryRow(ctx, `
			select updated_at from account_passwords where account_id = $1::uuid
		`, accountID).Scan(&stamped); err != nil {
			t.Fatalf("read updated_at: %v", err)
		}
		if stamped.Before(before) {
			t.Errorf("updated_at = %v, expected ≥ %v (now()-1s)", stamped, before)
		}
	})
}
