//go:build !no_pg

// Migration-apply test for 00285_data_upstreams_deployment_scope.sql
// (issue #954 / ADR-098-deployment-scope-overlay amendment).
//
// Pins:
//  1. The widened dedupe UNIQUE INDEX is on
//     (app_id, scope, deployment_scope, kind, host, port) —
//     a second INSERT with the same tuple but a different
//     deployment_scope succeeds (one per deployment), and a
//     third with all 5 keys matching fails with SQLSTATE
//     23505 on data_upstreams_dedupe_uniq. The ON CONFLICT
//     target in pkg/state/queries.sql::InsertDataUpstream
//     must mirror this index byte-for-byte.
//  2. The new column has the [a-z0-9-]{1,38}-style CHECK
//     (data_upstreams_deployment_scope_shape). INSERTs
//     outside the regex → 23514. The regex matches the
//     existing data_upstreams_scope_check shape and the
//     app_envs_scope_shape migration 00193 precedent.
//  3. The widened pg_notify pipe-payload carries 7 fields:
//     app_id|scope|deployment_scope|kind|host|port|op. The
//     harness LISTENs on db.NotifyDataUpstreamChanged,
//     inserts a row carrying a unique deployment_scope, and
//     asserts the payload ends in `<op>` (the TG_OP
//     sentinel) and contains the deployment_scope substring.
//     schedd's pkg/sched/listen.go parser relies on the
//     7-field layout — a regression to the 6-field format
//     breaks the wake side.
//
// Build tag matches the rest of the migration tests.
// Set FAAS_SKIP_PG_TESTS=1 to skip locally (see
// migrations/README.md).
package migrations_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00285_DataUpstreamsDeploymentScope(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// (0) Apply the full migration chain.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}

	// Seed account + app — the foreign-key targets for
	// every data_upstreams insert below. Distinct from
	// the per-test schema used by other migration tests
	// (each pgtest.Open mints a fresh random schema).
	accountID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO accounts (id, email, plan, status, created_at)
		VALUES ($1, $2, 'hobby', 'active', now())
	`, accountID, "pr-954-migtest@example.com"); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	appID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO apps (id, account_id, slug, type, runtime, ram_mb, max_concurrency, status)
		VALUES ($1, $2, 'pr-954-migtest', 'app', NULL, 256, 1, 'active')
	`, appID, accountID); err != nil {
		t.Fatalf("seed app: %v", err)
	}

	// (1) CHECK constraint rejects malformed deployment_scope.
	// 'NOT lowercase!' trips data_upstreams_deployment_scope_shape.
	_, err := pool.Exec(ctx, `
		INSERT INTO data_upstreams (
			id, account_id, app_id, source, scope, deployment_scope, kind, host, port,
			host_redacted_hash
		) VALUES (
			gen_random_uuid(), $1, $2, 'inferred', 'default', 'NOT lowercase!',
			'postgres', 'db-954-shape.example.com', 5432, $3
		)
	`, accountID, appID, dataUpstreamsHostRedactedSentinel)
	assertCheckViolation(t, err, "data_upstreams_deployment_scope_shape")

	// (2) Widened UNIQUE tripwire: a second INSERT with the
	// same (app_id, scope, deployment_scope, kind, host, port)
	// fails with 23505 on data_upstreams_dedupe_uniq. The
	// handler's ON CONFLICT target must mirror this index
	// byte-for-byte; a regression that drops the widening
	// breaks the dedupe-merge path.
	hostDedupe := "db-954-dedupe.example.com"

	// First row succeeds.
	if _, err := pool.Exec(ctx, `
		INSERT INTO data_upstreams (
			id, account_id, app_id, source, scope, deployment_scope, kind, host, port,
			host_redacted_hash
		) VALUES (
			gen_random_uuid(), $1, $2, 'inferred', 'default', 'default',
			'postgres', $3, 5432, $4
		)
	`, accountID, appID, hostDedupe, dataUpstreamsHostRedactedSentinel); err != nil {
		t.Fatalf("first dedupe-uniq insert: %v", err)
	}

	// Second row with the same 5-tuple (app_id, scope,
	// deployment_scope, kind, host, port) → 23505 on
	// data_upstreams_dedupe_uniq.
	_, err = pool.Exec(ctx, `
		INSERT INTO data_upstreams (
			id, account_id, app_id, source, scope, deployment_scope, kind, host, port,
			host_redacted_hash
		) VALUES (
			gen_random_uuid(), $1, $2, 'inferred', 'default', 'default',
			'postgres', $3, 5432, $4
		)
	`, accountID, appID, hostDedupe, dataUpstreamsHostRedactedSentinel)
	if err == nil {
		t.Fatal("second INSERT with same (app_id, scope, deployment_scope, kind, host, port) succeeded; data_upstreams_dedupe_uniq missing or regressed (handler ON CONFLICT path broken)")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("second dedupe-uniq insert: got non-Postgres error: %v", err)
	}
	if pgErr.Code != "23505" {
		t.Errorf("second dedupe-uniq insert: got SQLSTATE=%s, want 23505", pgErr.Code)
	}
	if pgErr.ConstraintName != "data_upstreams_dedupe_uniq" {
		t.Errorf("second dedupe-uniq insert: got constraint=%q, want data_upstreams_dedupe_uniq (handler ON CONFLICT target by name)", pgErr.ConstraintName)
	}

	// Third row with the same 4-tuple but a DIFFERENT
	// deployment_scope ('staging') succeeds — staging
	// and 'default' own distinct rows after the widening.
	if _, err := pool.Exec(ctx, `
		INSERT INTO data_upstreams (
			id, account_id, app_id, source, scope, deployment_scope, kind, host, port,
			host_redacted_hash
		) VALUES (
			gen_random_uuid(), $1, $2, 'inferred', 'default', 'staging',
			'postgres', $3, 5432, $4
		)
	`, accountID, appID, hostDedupe, dataUpstreamsHostRedactedSentinel); err != nil {
		t.Errorf("staging-deployment dedupe-key insert (deployment_scope='staging') should succeed alongside the 'default' row; got %v (the UNIQUE widening may have landed without the deployment_scope column)", err)
	}

	// (3) pg_notify payload widened to 7 fields. Acquire the
	// subscription BEFORE issuing writes (LISTEN must park on
	// the channel before pg_notify fires). Insert a row with
	// a unique host + deployment_scope and assert the payload
	// carries the deployment_scope substring and the row-op
	// suffix (the 7th field).
	notif, cancel, err := db.Subscribe(ctx, pool, []string{db.NotifyDataUpstreamChanged})
	if err != nil {
		t.Fatalf("Subscribe(data_upstreams_changed): %v", err)
	}
	defer cancel()

	notifHost := "db-954-pipe.example.com"
	notifDeploymentScope := "production"
	if _, err := pool.Exec(ctx, `
		INSERT INTO data_upstreams (
			id, account_id, app_id, source, scope, deployment_scope, kind, host, port,
			host_redacted_hash
		) VALUES (
			gen_random_uuid(), $1, $2, 'inferred', 'default', $3,
			'postgres', $4, 5432, $5
		)
	`, accountID, appID, notifDeploymentScope, notifHost, dataUpstreamsHostRedactedSentinel); err != nil {
		t.Fatalf("trigger test INSERT: %v", err)
	}

	got := waitForDataUpstreamNotification(t, notif, notifHost, 5*time.Second)
	if !strings.Contains(got.Payload, "INSERT") {
		t.Errorf("trigger INSERT payload missing INSERT: %q", got.Payload)
	}
	if !strings.Contains(got.Payload, notifDeploymentScope) {
		t.Errorf("trigger INSERT payload missing deployment_scope=%q (7-field pipe widening not active): %q", notifDeploymentScope, got.Payload)
	}
	// 7-field payload has exactly 7 pipe-delimited tokens
	// (the op is the 7th; the 6 field tokens come first).
	if gotToks := strings.Count(got.Payload, "|"); gotToks != 6 {
		t.Errorf("trigger INSERT payload pipe count: got %d '|' tokens (want 6 -> 7-field layout); full payload=%q", gotToks, got.Payload)
	}

	// Cleanup so the apply_walk second pass finds a clean
	// slate (the harness runs every test in isolation, but
	// belt-and-braces mirrors 00226's last block).
	for _, h := range []string{hostDedupe, notifHost} {
		if _, err := pool.Exec(ctx, `DELETE FROM data_upstreams WHERE host = $1`, h); err != nil {
			t.Fatalf("cleanup %s: %v", h, err)
		}
	}
}
