//go:build !no_pg

// Migration-apply test for 00045 (per-app autoscale target columns,
// issue #169 / #172). Pins the column shape + CHECK constraints:
//
//  1. Migration applies cleanly through 00045.
//  2. The new columns default to NULL (no implicit 0).
//  3. autoscale_target_rps accepts 0 (the explicit-disable sentinel
//     the wire uses; rejected by the OLD `apps_autoscale_target_rps_positive`
//     constraint, accepted by the new `apps_autoscale_target_rps_nonneg`).
//  4. autoscale_target_cpu_pct accepts 0 (the explicit-disable sentinel)
//     AND values in [1, 100]; values outside that range still fail
//     23514 against `apps_autoscale_target_cpu_pct_range`.
//  5. Negative values for either column are rejected — the apid
//     handler relies on these as the DB-level last-line-of-defence.
//
// Slot note: PR #229 originally planned 00044, but origin/main landed
// `00044_recent_build_claims.sql` after the plan was approved. 00045
// is the next free slot. The migration is slot-agnostic — only the
// filename and the test function name carry the literal.

package migrations_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00045_AppsAutoscale(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// (1) Apply through 00045.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 1 and 45)", err)
	}

	// (2) Seed an account + an app row that opts out of autoscale
	// (both columns NULL).
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-000000000045',
		        'autoscale-test@example.com', 'pro', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, status, created_at)
		values ('00000000-0000-0000-0000-000000000045',
		        '00000000-0000-0000-0000-000000000045',
		        'autoscale-default', 'app', 256, 5, 'active', now())
	`); err != nil {
		t.Fatalf("seed app: %v", err)
	}

	// (2) Both columns default to NULL.
	var rps, cpu *int
	if err := pool.QueryRow(ctx, `
		select autoscale_target_rps, autoscale_target_cpu_pct
		  from apps
		 where id = '00000000-0000-0000-0000-000000000045'
	`).Scan(&rps, &cpu); err != nil {
		t.Fatalf("read defaults: %v", err)
	}
	if rps != nil || cpu != nil {
		t.Fatalf("default columns: rps=%v cpu=%v, want both NULL", rps, cpu)
	}

	// (3) autoscale_target_rps = 0 is the explicit-disable sentinel.
	// The OLD constraint `apps_autoscale_target_rps_positive` rejected
	// this; the new `apps_autoscale_target_rps_nonneg` accepts it.
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, status,
		                  autoscale_target_rps, created_at)
		values ('00000000-0000-0000-0000-000000000145',
		        '00000000-0000-0000-0000-000000000045',
		        'autoscale-rps-zero', 'app', 256, 5, 'active', 0, now())
	`); err != nil {
		t.Fatalf("autoscale_target_rps=0 must be accepted (explicit-disable): %v", err)
	}

	// (4a) autoscale_target_cpu_pct = 0 is also the explicit-disable
	// sentinel — the OLD constraint rejected this; the new one accepts.
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, status,
		                  autoscale_target_cpu_pct, created_at)
		values ('00000000-0000-0000-0000-000000000245',
		        '00000000-0000-0000-0000-000000000045',
		        'autoscale-cpu-zero', 'app', 256, 5, 'active', 0, now())
	`); err != nil {
		t.Fatalf("autoscale_target_cpu_pct=0 must be accepted (explicit-disable): %v", err)
	}

	// (4b) Values in [1, 100] are accepted.
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, status,
		                  autoscale_target_cpu_pct, created_at)
		values ('00000000-0000-0000-0000-000000000345',
		        '00000000-0000-0000-0000-000000000045',
		        'autoscale-cpu-ok', 'app', 256, 5, 'active', 70, now())
	`); err != nil {
		t.Fatalf("autoscale_target_cpu_pct=70 must be accepted: %v", err)
	}

	// (4c) cpu > 100 must be rejected with 23514 against cpu_pct_range.
	_, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, status,
		                  autoscale_target_cpu_pct, created_at)
		values ('00000000-0000-0000-0000-000000000445',
		        '00000000-0000-0000-0000-000000000045',
		        'autoscale-cpu-too-high', 'app', 256, 5, 'active', 150, now())
	`)
	if err == nil {
		t.Fatalf("autoscale_target_cpu_pct=150 must be rejected; apps_autoscale_target_cpu_pct_range did not fire")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("cpu>100 error not a *pgconn.PgError: %v", err)
	}
	if pgErr.Code != "23514" {
		t.Errorf("cpu>100 SQLSTATE = %q, want 23514 (check_violation); full: %v", pgErr.Code, err)
	}
	if pgErr.ConstraintName != "apps_autoscale_target_cpu_pct_range" {
		t.Errorf("cpu>100 constraint = %q, want apps_autoscale_target_cpu_pct_range; full: %v", pgErr.ConstraintName, err)
	}

	// (5) Negative rps is rejected by the new nonneg constraint.
	_, err = pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, status,
		                  autoscale_target_rps, created_at)
		values ('00000000-0000-0000-0000-000000000545',
		        '00000000-0000-0000-0000-000000000045',
		        'autoscale-rps-neg', 'app', 256, 5, 'active', -1, now())
	`)
	if err == nil {
		t.Fatalf("autoscale_target_rps=-1 must be rejected; apps_autoscale_target_rps_nonneg did not fire")
	}
	if !errors.As(err, &pgErr) {
		t.Fatalf("rps<0 error not a *pgconn.PgError: %v", err)
	}
	if pgErr.Code != "23514" {
		t.Errorf("rps<0 SQLSTATE = %q, want 23514; full: %v", pgErr.Code, err)
	}
	if pgErr.ConstraintName != "apps_autoscale_target_rps_nonneg" {
		t.Errorf("rps<0 constraint = %q, want apps_autoscale_target_rps_nonneg; full: %v", pgErr.ConstraintName, err)
	}
}
