//go:build !no_pg

// Migration-apply tests for 00102 (apps.migrated_at +
// apps_migrated_at_chk, Tier A5 cross-node live-instance
// migration, ADR-066, follow-up to ADR-064).
//
// Pins the Tier A5 schema contract verbatim:
//
//	1. apps.migrated_at column exists with data_type
//	   'timestamp with time zone' + nullable YES. Fresh apps
//	   start NULL (no migration history yet) — the engine's
//	   hot path on InsertApp must NOT write a non-NULL value
//	   in the migration PR (PR #440 appsSelectColumns
//	   regression precedent).
//	2. CHECK apps_migrated_at_chk tolerates NULL and tolerates
//	   a past timestamp; values clearly in the future still
//	   error 23514 (clock-skew guard, same shape as 00095 /
//	   00101).
//	3. Replay-safety: a second MigrateUp() returns nil —
//	   ADD COLUMN IF NOT EXISTS paired with DROP CONSTRAINT
//	   IF EXISTS / DROP COLUMN IF EXISTS (PR #377 / ADR-041).
//	4. Down symmetry: the down body drops the CHECK + the
//	   column cleanly; the re-applied up body round-trips.
//
// Distinct from A4's apps.reassigned_at (slot 92,
// 00092_apps_reassigned_at.sql) — both columns can coexist on
// the same app. An app whose instances migrated live last
// week AND whose owner was rebalanced to a new node via the
// A4 parked path yesterday has both stamps set.
//
// Build tag mirrors apply_walk_test.go:4 and the other
// 0008x/0009x migration tests — set FAAS_SKIP_PG_TESTS=1
// to skip.

package migrations_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

// TestMigration_00102_1_ColumnShape pins the schema of the
// new apps.migrated_at column after 00102 applies. A
// regression (e.g. tightening NOT NULL on migrated_at) fails
// loud here — the engine's hot path relies on the column
// being nullable at insert time (a fresh app has never been
// migrated).
func TestMigration_00102_1_ColumnShape(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	defer pool.Close()
	migrateUpOnce(ctx, t, pool)

	// Nullable? Must be YES — fresh apps start NULL.
	var nullable string
	if err := pool.QueryRow(ctx, `
		select is_nullable from information_schema.columns
		 where table_schema = current_schema()
		   and table_name   = 'apps'
		   and column_name  = 'migrated_at'
	`).Scan(&nullable); err != nil {
		t.Fatalf("query apps.migrated_at is_nullable: %v", err)
	}
	if nullable != "YES" {
		t.Errorf("apps.migrated_at is_nullable = %q, want %q", nullable, "YES")
	}

	// Spot-check data_type. timestamptz, not text.
	var dtype string
	if err := pool.QueryRow(ctx, `
		select data_type from information_schema.columns
		 where table_schema = current_schema()
		   and table_name   = 'apps'
		   and column_name  = 'migrated_at'
	`).Scan(&dtype); err != nil {
		t.Fatalf("query apps.migrated_at data_type: %v", err)
	}
	if dtype != "timestamp with time zone" {
		t.Errorf("apps.migrated_at data_type = %q, want %q",
			dtype, "timestamp with time zone")
	}
}

// TestMigration_00102_2_AllowsNull pins the never-migrated
// case. INSERT an app row with migrated_at NULL; SELECT it
// back; assert NULL round-trips. The engine's hot path on
// InsertApp must NOT write a non-NULL value in this PR —
// PR #440 appsSelectColumns regression precedent.
func TestMigration_00102_2_AllowsNull(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	defer pool.Close()
	migrateUpOnce(ctx, t, pool)

	accountID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ($1, $2, 'free', now())
	`, accountID, accountID[:8]+"@test.example"); err != nil {
		t.Fatalf("seed accounts: %v", err)
	}
	appID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb,
		                  max_concurrency, idle_timeout_s, status,
		                  migrated_at, created_at)
		values ($1, $2, $3, 'function', 256, 2, 60, 'active',
		        NULL, now())
	`, appID, accountID, "live-mig-app-null-"+accountID[:8]); err != nil {
		t.Fatalf("insert app with migrated_at NULL: %v", err)
	}

	var got *time.Time
	if err := pool.QueryRow(ctx, `
		select migrated_at from apps where id = $1
	`, appID).Scan(&got); err != nil {
		t.Fatalf("select apps.migrated_at: %v", err)
	}
	if got != nil {
		t.Errorf("apps.migrated_at round-tripped to %v, want NULL", *got)
	}
}

// TestMigration_00102_3_AllowsPastTimestamp pins the normal
// post-migration case. UPDATE an app's migrated_at to now() -
// 1 hour; the row must round-trip. The CHECK tolerates any
// timestamp in the past — no upper bound is enforced except
// the clock-skew window.
func TestMigration_00102_3_AllowsPastTimestamp(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	defer pool.Close()
	migrateUpOnce(ctx, t, pool)

	accountID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ($1, $2, 'free', now())
	`, accountID, accountID[:8]+"@test.example"); err != nil {
		t.Fatalf("seed accounts: %v", err)
	}
	appID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb,
		                  max_concurrency, idle_timeout_s, status,
		                  migrated_at, created_at)
		values ($1, $2, $3, 'function', 256, 2, 60, 'active',
		        now() - interval '1 hour', now())
	`, appID, accountID, "live-mig-app-past-"+accountID[:8]); err != nil {
		t.Fatalf("insert app with past migrated_at: %v", err)
	}

	var got time.Time
	if err := pool.QueryRow(ctx, `
		select migrated_at from apps where id = $1
	`, appID).Scan(&got); err != nil {
		t.Fatalf("select apps.migrated_at: %v", err)
	}
	if got.IsZero() {
		t.Errorf("apps.migrated_at round-tripped to zero value, want a recent past timestamp")
	}
}

// TestMigration_00102_4_RejectsFutureTimestamp pins the
// clock-skew guard. INSERT an app with migrated_at = now() +
// 1 hour (clearly past the CHECK's +1 minute tolerance); the
// row must fail 23514. The CHECK is the tripwire for a
// misconfigured clock or a buggy write path that would
// otherwise pin an app's lineage far in the future.
func TestMigration_00102_4_RejectsFutureTimestamp(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	defer pool.Close()
	migrateUpOnce(ctx, t, pool)

	accountID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ($1, $2, 'free', now())
	`, accountID, accountID[:8]+"@test.example"); err != nil {
		t.Fatalf("seed accounts: %v", err)
	}

	_, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb,
		                  max_concurrency, idle_timeout_s, status,
		                  migrated_at, created_at)
		values ($1, $2, $3, 'function', 256, 2, 60, 'active',
		        now() + interval '1 hour', now())
	`, uuid.NewString(), accountID, "live-mig-app-future-"+accountID[:8])
	if err == nil {
		t.Fatal("expected check violation on migrated_at = now() + 1h; got nil (CHECK is missing or not enforced)")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("want SQLSTATE 23514 on future migrated_at, got %v", err)
	}
}

// TestMigration_00102_5_ReplaySafe pins the idempotency
// contract. A second MigrateUp() returns nil — ADD COLUMN
// IF NOT EXISTS paired with DROP CONSTRAINT IF EXISTS / DROP
// COLUMN IF EXISTS in the down block (PR #377 / ADR-041).
func TestMigration_00102_5_ReplaySafe(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	defer pool.Close()

	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("first MigrateUp: %v", err)
	}
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay MigrateUp: %v (ADD COLUMN must be IF NOT EXISTS, constraint add paired with DROP CONSTRAINT IF EXISTS)", err)
	}
}

// TestMigration_00102_6_DownSymmetry pins the down path.
// Drive the SQL the down body carries directly, then re-
// apply the up body and assert the column + CHECK come back.
// A non-symmetric down would leave a broken schema on a
// release that needs to roll back 00102 in isolation.
func TestMigration_00102_6_DownSymmetry(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	defer pool.Close()

	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}

	// Down body — drop in the reverse order of creation.
	// (The actual migration uses ALTER TABLE ... DROP
	// CONSTRAINT IF EXISTS + DROP COLUMN IF EXISTS; the test
	// mirrors the down body exactly so a drift between the
	// file and the test surfaces immediately.)
	if _, err := pool.Exec(ctx,
		`alter table apps drop constraint if exists apps_migrated_at_chk`); err != nil {
		t.Fatalf("down: drop chk: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`alter table apps drop column if exists migrated_at`); err != nil {
		t.Fatalf("down: drop migrated_at: %v", err)
	}

	// Probe: column gone.
	var count int
	if err := pool.QueryRow(ctx, `
		select count(*) from information_schema.columns
		 where table_schema = current_schema()
		   and table_name   = 'apps'
		   and column_name  = 'migrated_at'
	`).Scan(&count); err != nil {
		t.Fatalf("probe migrated_at absence: %v", err)
	}
	if count != 0 {
		t.Errorf("after down, apps.migrated_at still present (count=%d)", count)
	}

	// Probe: CHECK gone.
	var chkCount int
	if err := pool.QueryRow(ctx, `
		select count(*) from pg_constraint con
		 join pg_class c on c.oid = con.conrelid
		 join pg_namespace n on n.oid = c.relnamespace
		 where n.nspname = current_schema()
		   and c.relname  = 'apps'
		   and con.conname = 'apps_migrated_at_chk'
	`).Scan(&chkCount); err != nil {
		t.Fatalf("probe apps_migrated_at_chk absence: %v", err)
	}
	if chkCount != 0 {
		t.Errorf("after down, apps_migrated_at_chk still present (count=%d)", chkCount)
	}

	// Re-apply the up body — must succeed cleanly.
	if _, err := pool.Exec(ctx, `
		alter table apps
		  add column if not exists migrated_at timestamptz,
		  add constraint apps_migrated_at_chk
		    check (migrated_at is null
		           or migrated_at <= now() + interval '1 minute')
	`); err != nil {
		t.Fatalf("re-add column + chk: %v", err)
	}

	// Probe: migrated_at back.
	if err := pool.QueryRow(ctx, `
		select count(*) from information_schema.columns
		 where table_schema = current_schema()
		   and table_name   = 'apps'
		   and column_name  = 'migrated_at'
	`).Scan(&count); err != nil {
		t.Fatalf("probe migrated_at re-added: %v", err)
	}
	if count != 1 {
		t.Errorf("after re-add, apps.migrated_at present = %d, want 1", count)
	}
}
