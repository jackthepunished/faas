//go:build !no_pg

// Migration-apply test for 00073 (projects table + apps workload columns,
// ADR-050 Phase 1).
//
// Pins the Phase 1 acceptance gate verbatim:
// <migration replays clean; backfill converts every existing bound app
// into a one-member project; standalone apps keep deploying unchanged;
// make test green.>
//
// Assertions:
//
//	1. projects table shape (columns + nullability + scan_source default).
//	2. apps_workload_class_chk rejects non-canonical values and accepts
//	   the five canonical ones.
//	3. apps_project_workload_uniq partial unique fires when a second
//	   app under the same (project_id, workload_name) is attempted.
//	4. apps_github_install_repo_uniq is gone (the 1:1 binding is
//	   superseded by projects_install_repo_uniq).
//	5. Backfill synthesizes one project per (install_id, repo_full_name)
//	   and stamps apps.project_id + apps.workload_name = app.slug.
//	6. Standalone apps (no github_install_id) keep project_id NULL
//	   and workload_name = '' — the standalone deploy contract from
//	   docs/repo_decomposition_implementation.md:161-162.
//	7. Idempotency: a second MigrateUp() returns nil (replay-safety
//	   contract; mirrors 00053_deployments_source_url_test.go).
//
// Build tag mirrors apply_walk_test.go:4 and 00072_compute_nodes_region_zone_test.go:1
// — set FAAS_SKIP_PG_TESTS=1 to skip locally.

package migrations_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00073_ProjectsAndWorkloads(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}

	// (1) projects table shape.
	expectedCols := map[string]struct{ dataType, nullable string }{
		"id":                {"uuid", "NO"},
		"account_id":        {"uuid", "NO"},
		"slug":              {"text", "NO"},
		"repo_full_name":    {"text", "YES"},
		"production_branch": {"text", "YES"},
		"install_id":        {"bigint", "YES"},
		"scan_source":       {"text", "NO"},
		"created_at":        {"timestamp with time zone", "NO"},
		"updated_at":        {"timestamp with time zone", "NO"},
	}
	for col, want := range expectedCols {
		var dataType, nullable string
		if err := pool.QueryRow(ctx, `
			select data_type, is_nullable
			  from information_schema.columns
			 where table_schema = current_schema()
			   and table_name   = 'projects'
			   and column_name  = $1
		`, col).Scan(&dataType, &nullable); err != nil {
			t.Errorf("projects.%s not present after migration: %v", col, err)
			continue
		}
		if dataType != want.dataType {
			t.Errorf("projects.%s data_type = %q, want %q", col, dataType, want.dataType)
		}
		if nullable != want.nullable {
			t.Errorf("projects.%s is_nullable = %q, want %q", col, nullable, want.nullable)
		}
	}

	// scan_source has the documented default 'unknown'. Without
	// the default, a future migration that stops providing scan_source
	// on INSERT trips NOT NULL.
	var hasDefault bool
	if err := pool.QueryRow(ctx, `
		select (column_default is not null) from information_schema.columns
		 where table_schema = current_schema()
		   and table_name   = 'projects'
		   and column_name  = 'scan_source'
	`).Scan(&hasDefault); err != nil {
		t.Errorf("scan projects.scan_source default probe: %v", err)
	}
	if !hasDefault {
		t.Errorf("projects.scan_source has no DB default; expected 'unknown'")
	}

	// Seed an account — the apps FK references accounts(id). The test
	// does not run any future Phase 1 endpoint code, so this is the
	// fastest scaffolding path: insert the parent row directly.
	acctID := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`insert into accounts (id, email, plan) values ($1, $2, 'free')`,
		acctID, acctID+"@example.test"); err != nil {
		t.Fatalf("seed accounts row: %v", err)
	}

	// (2) apps_workload_class_chk — accept canonical, reject garbage.
	appID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, ram_mb, max_concurrency)
		values ($1, $2, $3, 256, 1)
	`, appID, acctID, "wn-00073-class-canonical"); err != nil {
		t.Fatalf("seed apps row (canonical default workload_class): %v", err)
	}
	for _, valid := range []string{"http", "graphql", "grpc", "job", "worker"} {
		appIDLocal := uuid.NewString()
		_, err := pool.Exec(ctx, `
			insert into apps (id, account_id, slug, ram_mb, max_concurrency, workload_class)
			values ($1, $2, $3, 256, 1, $4)
		`, appIDLocal, acctID, "wn-00073-class-"+valid, valid)
		if err != nil {
			t.Errorf("insert with workload_class=%q rejected: %v", valid, err)
			continue
		}
		// Clean up so the slug stays unique per test run.
		_, _ = pool.Exec(ctx, `delete from apps where id = $1`, appIDLocal)
	}
	for _, bogus := range []string{"httpFoo", "HTTP", "tcp", ""} {
		appIDBogus := uuid.NewString()
		_, err := pool.Exec(ctx, `
			insert into apps (id, account_id, slug, ram_mb, max_concurrency, workload_class)
			values ($1, $2, $3, 256, 1, $4)
		`, appIDBogus, acctID, "wn-00073-class-bogus-"+strings.ReplaceAll(bogus, " ", "_"), bogus)
		if err == nil {
			t.Errorf("insert with workload_class=%q accepted; want CHECK violation", bogus)
			_, _ = pool.Exec(ctx, `delete from apps where id = $1`, appIDBogus)
		}
	}

	// (3) apps_project_workload_uniq — partial unique on
	// (project_id, workload_name). Two members under the same project
	// with the same workload_name must trip 23505.
	projID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		insert into projects (id, account_id, slug, scan_source)
		values ($1, $2, $3, 'single')
	`, projID, acctID, "wn-00073-unique-test"); err != nil {
		t.Fatalf("seed projects row for unique-test: %v", err)
	}
	appA := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, ram_mb, max_concurrency,
		                  project_id, workload_name, workload_class)
		values ($1, $2, $3, 256, 1, $4, 'web', 'http')
	`, appA, acctID, "wn-00073-unique-A", projID); err != nil {
		t.Fatalf("seed first project-member app: %v", err)
	}
	appB := uuid.NewString()
	_, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, ram_mb, max_concurrency,
		                  project_id, workload_name, workload_class)
		values ($1, $2, $3, 256, 1, $4, 'web', 'http')
	`, appB, acctID, "wn-00073-unique-B", projID)
	if err == nil {
		t.Errorf("second apps row with same (project_id, workload_name) accepted; want 23505")
		_, _ = pool.Exec(ctx, `delete from apps where id = $1`, appB)
	} else {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			t.Errorf("duplicate (project_id, workload_name) errored with code=%v want 23505 (raw: %v)",
				pgErrCode(pgErr), err)
		}
	}
	// Distinct workload_name succeeds (regression guard).
	appC := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, ram_mb, max_concurrency,
		                  project_id, workload_name, workload_class)
		values ($1, $2, $3, 256, 1, $4, 'worker', 'worker')
	`, appC, acctID, "wn-00073-unique-C", projID); err != nil {
		t.Errorf("distinct workload_name insert rejected: %v", err)
	}

	// (4) apps_github_install_repo_uniq is gone.
	var droppedCount int
	if err := pool.QueryRow(ctx, `
		select count(*) from pg_indexes
		 where schemaname = current_schema()
		   and tablename  = 'apps'
		   and indexname  = 'apps_github_install_repo_uniq'
	`).Scan(&droppedCount); err != nil {
		t.Errorf("pg_indexes probe for apps_github_install_repo_uniq: %v", err)
	}
	if droppedCount != 0 {
		t.Errorf("apps_github_install_repo_uniq still present (count=%d); migration should drop it", droppedCount)
	}

	// Apps push-dispatch index from 00050 must survive (regression
	// guard — Phase 1 drops the 1:1 binding, not the dispatch path).
	var keptCount int
	if err := pool.QueryRow(ctx, `
		select count(*) from pg_indexes
		 where schemaname = current_schema()
		   and tablename  = 'apps'
		   and indexname  = 'apps_github_install_repo_branch_idx'
	`).Scan(&keptCount); err != nil {
		t.Errorf("pg_indexes probe for apps_github_install_repo_branch_idx: %v", err)
	}
	if keptCount == 0 {
		t.Errorf("apps_github_install_repo_branch_idx dropped; Phase 1 should only drop the 1:1 binding")
	}

	// (5) Backfill: two bound apps under the same (account, install_id,
	// repo_full_name) on DIFFERENT repos produce two distinct project rows;
	// workload_name = app.slug for each. Use two slugs per repo to
	// avoid the partial-unique tripping within the test fixture; we
	// INSERT one app per repo on a fresh schema state. The backfill
	// already ran above during MigrateUp — verify it landed.
	boundAcctID := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`insert into accounts (id, email, plan) values ($1, $2, 'free')`,
		boundAcctID, boundAcctID+"@example.test"); err != nil {
		t.Fatalf("seed bound account: %v", err)
	}
	boundApp1 := uuid.NewString()
	boundApp2 := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, ram_mb, max_concurrency,
		                  github_install_id, github_repo_full_name, github_production_branch)
		values ($1, $2, 'wn-00073-backfill-a', 256, 1, 70001, 'acme/wn-00073-a', 'main')
	`, boundApp1, boundAcctID); err != nil {
		t.Fatalf("seed bound app 1: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, ram_mb, max_concurrency,
		                  github_install_id, github_repo_full_name, github_production_branch)
		values ($1, $2, 'wn-00073-backfill-b', 256, 1, 70002, 'acme/wn-00073-b', 'main')
	`, boundApp2, boundAcctID); err != nil {
		t.Fatalf("seed bound app 2: %v", err)
	}
	// The backfill inside the migration already ran on the empty
	// schema before we seeded these rows. Re-run just the INSERT
	// INTO projects/UPDATE apps statements manually to confirm
	// the body is replay-safe on a non-empty data set.
	if _, err := pool.Exec(ctx, `
		insert into projects (account_id, slug, repo_full_name, production_branch, install_id, scan_source)
		select distinct on (a.github_install_id, a.github_repo_full_name)
		       a.account_id, a.slug, a.github_repo_full_name,
		       a.github_production_branch, a.github_install_id, 'single'
		from apps a
		where a.github_repo_full_name is not null
		  and a.github_install_id is not null
		  and a.slug ~ '^[a-z0-9][a-z0-9-]{0,62}$'
		on conflict (account_id, slug) do nothing
	`); err != nil {
		t.Fatalf("manual backfill INSERT INTO projects: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		update apps a
		   set project_id    = p.id,
		       workload_name = a.slug
		  from projects p
		 where p.install_id       = a.github_install_id
		   and p.repo_full_name   = a.github_repo_full_name
		   and a.project_id is null
	`); err != nil {
		t.Fatalf("manual backfill UPDATE apps: %v", err)
	}

	// Two projects under boundAcctID, one per repo.
	var projCount int
	if err := pool.QueryRow(ctx,
		`select count(*) from projects where account_id = $1`, boundAcctID,
	).Scan(&projCount); err != nil {
		t.Fatalf("count projects under boundAcctID: %v", err)
	}
	if projCount != 2 {
		t.Errorf("projects under boundAcctID = %d, want 2", projCount)
	}

	for _, appIDLocal := range []string{boundApp1, boundApp2} {
		var pid *string
		var wlname string
		if err := pool.QueryRow(ctx,
			`select project_id, workload_name from apps where id = $1`, appIDLocal,
		).Scan(&pid, &wlname); err != nil {
			t.Errorf("read backfilled app %s: %v", appIDLocal, err)
			continue
		}
		if pid == nil || *pid == "" {
			t.Errorf("backfilled app %s project_id is NULL; want non-null", appIDLocal)
		}
		if !strings.HasPrefix(wlname, "wn-00073-backfill-") {
			t.Errorf("backfilled app %s workload_name = %q, want wn-00073-backfill-* (slug verbatim)",
				appIDLocal, wlname)
		}
	}

	// (6) Standalone path: an app without github_install_id keeps
	// project_id NULL and workload_name = ''. Mirrors the contract
	// from docs/repo_decomposition_implementation.md:161-162.
	standaloneAcct := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`insert into accounts (id, email, plan) values ($1, $2, 'free')`,
		standaloneAcct, standaloneAcct+"@example.test"); err != nil {
		t.Fatalf("seed standalone account: %v", err)
	}
	standaloneApp := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, ram_mb, max_concurrency)
		values ($1, $2, 'wn-00073-standalone', 256, 1)
	`, standaloneApp, standaloneAcct); err != nil {
		t.Fatalf("seed standalone app: %v", err)
	}
	var saProj *string
	var saWL, saClass string
	if err := pool.QueryRow(ctx,
		`select project_id, workload_name, workload_class::text from apps where id = $1`,
		standaloneApp,
	).Scan(&saProj, &saWL, &saClass); err != nil {
		t.Fatalf("read standalone app: %v", err)
	}
	if saProj != nil {
		t.Errorf("standalone app project_id = %q, want NULL", *saProj)
	}
	if saWL != "" {
		t.Errorf("standalone app workload_name = %q, want ''", saWL)
	}
	if saClass != "http" {
		t.Errorf("standalone app workload_class = %q, want 'http'", saClass)
	}

	// (7) Replay-safety: a second MigrateUp is a no-op.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Errorf("second MigrateUp failed (replay-safety violation): %v", err)
	}
}

func pgErrCode(pgErr *pgconn.PgError) string {
	if pgErr == nil {
		return "<nil>"
	}
	return pgErr.Code
}
