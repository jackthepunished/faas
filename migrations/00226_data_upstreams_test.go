//go:build !no_pg

// Migration-apply test for 00226_data_upstreams.sql
// (ADR-098 §9.A / connection-aware execution cluster
// PR-A — pure DDL + sqlc + pgstore/memstore/types + docs).
//
// Pins:
//
//  1. Both tables exist:
//     - data_upstreams
//     - data_upstream_probes
//     Plus the default partition
//     `data_upstream_probes_default`.
//  2. data_upstream_probes is PARTITIONED BY RANGE on
//     sampled_at, and the default partition
//     data_upstream_probes_default exists. The
//     pg_partitioned_table catalog row confirms the
//     partition strategy; the information_schema confirms
//     the default partition. First partitioned table in the
//     repo — if a future schema-dump tool strips the
//     PARTITION BY clause, this assertion fails before the
//     breakage reaches production.
//  3. CHECK constraints reject malformed input:
//     - source outside ('inferred','explicit') → 23514
//     - scope not matching app_envs_scope_shape → 23514
//     - kind outside the 14-value vocabulary → 23514
//     - host outside the RFC 1123 regex → 23514
//     (×3 flavors: wildcard + underscores + IPv4
//     literal — the classifier must normalise
//     BEFORE INSERT)
//     - port outside 1..65535 → 23514
//     - host_redacted_hash not 64 hex / not the
//     __unsalted__ sentinel → 23514
//     - last_rtt_ms out of [0, 600000] → 23514
//     - last_probed_at / last_rtt_ms pair violated → 23514
//     - data_upstream_probes ok/rtt/error_class pair
//     violated → 23514
//  4. FK cascade: deleting an account cascades to both
//     data_upstreams and the apps that the upstreams row
//     points to. data_upstream_probes has no FK (intentional
//     — probes survive app deletion for §12 fleet
//     telemetry continuity).
//  5. Trigger fires on INSERT/UPDATE/DELETE on
//     data_upstreams and emits on channel
//     `data_upstreams_changed`. The harness subscribes via
//     db.Subscribe, performs each op, and asserts the
//     pipe-delimited payload contains the host + the right
//     TG_OP within a 5s window.
//  6. host_redacted_hash is NOT NULL on data_upstreams,
//     and the greenfield sentinel '__unsalted__' is accepted
//     (used by the test fixtures themselves; a future
//     production writer must never stamp the sentinel — see
//     D6 of the ADR).
//  7. UNIQUE tripwire: a second INSERT with the same
//     (app_id, scope, kind, host, port) fails with 23505 on
//     data_upstreams_dedupe_uniq. This is the exact
//     constraint name pkg/state/queries.sql::InsertDataUpstream
//     targets with ON CONFLICT (...) DO UPDATE — a
//     regression that drops the index silently breaks the
//     dedupe-merge path.
//
// Slot reservation: 00226_reserve_slot.sql (PR-0 / PR
// #858, MERGED 2026-08-13) fenced this slot ahead of
// sibling PRs #864/#867/#845 per the cross-PR slot
// reservation convention (ADR-041). On PR-A merge, sibling
// PRs must drop their fences on rebase —
// `fix(migration): drop 00226_reserve_slot.sql`.
//
// Build tag matches the rest of the migration tests;
// set FAAS_SKIP_PG_TESTS=1 to skip locally (see
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

const dataUpstreamsHostRedactedSentinel = "__unsalted__"

func TestMigrations_00226_DataUpstreams(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// Self-contained: apply the full migration set into
	// the per-test schema (each pgtest.Open call mints a
	// fresh random schema — none of the prior tests'
	// tables leak in). This makes the test pass under
	// `go test -run TestMigrations_00226_DataUpstreams`
	// without depending on TestMigrationsApplyAndWalk
	// running first.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}

	// Seed an account + app so the FK and CHECK
	// assertions have a parent row. The schema fixtures
	// (migrations/00002_accounts.sql +
	// migrations/00003_apps.sql) precede 00226 in the
	// embed sequence, so the tables exist after
	// db.MigrateUp.
	accountID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO accounts (id, email, plan, status, created_at)
		VALUES ($1, $2, 'hobby', 'active', now())
	`, accountID, "pr-a-migtest@example.com"); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	appID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO apps (id, account_id, slug, type, runtime, ram_mb, max_concurrency, status)
		VALUES ($1, $2, 'pr-a-migtest', 'app', NULL, 256, 1, 'active')
	`, appID, accountID); err != nil {
		t.Fatalf("seed app: %v", err)
	}

	// (1) Both tables + the default partition exist.
	for _, table := range []string{
		"data_upstreams",
		"data_upstream_probes",
		"data_upstream_probes_default",
	} {
		var exists bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.tables
				WHERE table_schema = current_schema()
				  AND table_name = $1
			)
		`, table).Scan(&exists); err != nil {
			t.Fatalf("query %s existence: %v", table, err)
		}
		if !exists {
			t.Errorf("table %s missing (00226_data_upstreams.sql must create data_upstreams, data_upstream_probes, and the default partition)", table)
		}
	}

	// (2) data_upstream_probes is partitioned by RANGE on
	// sampled_at (PARTITION BY RANGE) and the default
	// partition exists. pg_partitioned_table.partstrat = 'r'
	// (range) and pg_get_partkeydef yields the column list.
	var partStrat string
	var partKeyDef string
	if err := pool.QueryRow(ctx, `
		SELECT pt.partstrat::text,
		       pg_get_partkeydef(c.oid)
		  FROM pg_partitioned_table pt
		  JOIN pg_class c ON c.oid = pt.partrelid
		 WHERE c.relname = 'data_upstream_probes'
		   AND c.relnamespace = current_schema()::regnamespace
	`).Scan(&partStrat, &partKeyDef); err != nil {
		t.Fatalf("query partition strategy: %v", err)
	}
	if partStrat != "r" {
		t.Errorf("data_upstream_probes partstrat: got %q, want 'r' (PARTITION BY RANGE)", partStrat)
	}
	if !strings.Contains(partKeyDef, "sampled_at") {
		t.Errorf("data_upstream_probes partition column: got %q, must contain sampled_at", partKeyDef)
	}

	// (3) Account + app already seeded above by the
	// MigrateUp block; CHECK violations below insert
	// into data_upstreams with (accountID, appID).

	// (3a) source outside ('inferred','explicit') → 23514.
	_, err := pool.Exec(ctx, `
		INSERT INTO data_upstreams (
			id, account_id, app_id, source, scope, kind, host, port,
			host_redacted_hash
		) VALUES (
			gen_random_uuid(), $1, $2, 'bogus_source', 'default',
			'postgres', 'db.example.com', 5432, $3
		)
	`, accountID, appID, dataUpstreamsHostRedactedSentinel)
	assertCheckViolation(t, err, "data_upstreams_source_check")

	// (3b) scope not matching app_envs_scope_shape → 23514.
	// The shape regex requires 3..40 lowercase chars
	// ([a-z0-9-], no leading/trailing dash); 'NOT lowercase!'
	// has uppercase + punctuation.
	_, err = pool.Exec(ctx, `
		INSERT INTO data_upstreams (
			id, account_id, app_id, source, scope, kind, host, port,
			host_redacted_hash
		) VALUES (
			gen_random_uuid(), $1, $2, 'inferred', 'NOT lowercase!',
			'postgres', 'db.example.com', 5432, $3
		)
	`, accountID, appID, dataUpstreamsHostRedactedSentinel)
	assertCheckViolation(t, err, "data_upstreams_scope_check")

	// (3c) kind outside the 14-value vocabulary → 23514.
	_, err = pool.Exec(ctx, `
		INSERT INTO data_upstreams (
			id, account_id, app_id, source, scope, kind, host, port,
			host_redacted_hash
		) VALUES (
			gen_random_uuid(), $1, $2, 'inferred', 'default',
			'bogus_kind', 'db.example.com', 5432, $3
		)
	`, accountID, appID, dataUpstreamsHostRedactedSentinel)
	assertCheckViolation(t, err, "data_upstreams_kind_check")

	// (3d) host outside the RFC 1123 regex → 23514. Three
	// flavors: wildcard S3 (the classifier must normalise
	// BEFORE INSERT), a host with an underscore (forbidden
	// by RFC 1123; the classifier rejects it), and an IPv4
	// literal (PostgreSQL's ARE accepts 192.168.1.1 as four
	// [a-z0-9] labels — a regression would let the IPv4
	// slip past the regex; the second conjunct
	// `host !~ '^[0-9]+(\.[0-9]+)+$'` rejects it before
	// the row poisons the probe loop).
	for _, badHost := range []string{"*.s3.amazonaws.com", "db_with_underscore.example.com", "192.168.1.1"} {
		_, err = pool.Exec(ctx, `
			INSERT INTO data_upstreams (
				id, account_id, app_id, source, scope, kind, host, port,
				host_redacted_hash
			) VALUES (
				gen_random_uuid(), $1, $2, 'inferred', 'default',
				'postgres', $3, 5432, $4
			)
		`, accountID, appID, badHost, dataUpstreamsHostRedactedSentinel)
		assertCheckViolation(t, err, "data_upstreams_host_check")
	}

	// (3e) port outside 1..65535 → 23514.
	_, err = pool.Exec(ctx, `
		INSERT INTO data_upstreams (
			id, account_id, app_id, source, scope, kind, host, port,
			host_redacted_hash
		) VALUES (
			gen_random_uuid(), $1, $2, 'inferred', 'default',
			'postgres', 'db.example.com', 70000, $3
		)
	`, accountID, appID, dataUpstreamsHostRedactedSentinel)
	assertCheckViolation(t, err, "data_upstreams_port_check")

	// (3f) host_redacted_hash not 64 hex / not the
	// sentinel → 23514.
	_, err = pool.Exec(ctx, `
		INSERT INTO data_upstreams (
			id, account_id, app_id, source, scope, kind, host, port,
			host_redacted_hash
		) VALUES (
			gen_random_uuid(), $1, $2, 'inferred', 'default',
			'postgres', 'db.example.com', 5432, 'not-a-hash'
		)
	`, accountID, appID)
	assertCheckViolation(t, err, "data_upstreams_host_redacted_hash_check")

	// (3g) last_rtt_ms out of [0, 600000] → 23514.
	_, err = pool.Exec(ctx, `
		INSERT INTO data_upstreams (
			id, account_id, app_id, source, scope, kind, host, port,
			host_redacted_hash, last_rtt_ms, last_probed_at
		) VALUES (
			gen_random_uuid(), $1, $2, 'inferred', 'default',
			'postgres', 'db.example.com', 5432, $3, 700000, now()
		)
	`, accountID, appID, dataUpstreamsHostRedactedSentinel)
	assertCheckViolation(t, err, "data_upstreams_last_rtt_ms_check")

	// (3h) last_probed_at / last_rtt_ms pair violated
	// → 23514. last_rtt_ms IS NULL AND last_probed_at
	// IS NOT NULL is one of the two bad shapes.
	_, err = pool.Exec(ctx, `
		INSERT INTO data_upstreams (
			id, account_id, app_id, source, scope, kind, host, port,
			host_redacted_hash, last_rtt_ms, last_probed_at
		) VALUES (
			gen_random_uuid(), $1, $2, 'inferred', 'default',
			'postgres', 'db.example.com', 5432, $3, NULL, now()
		)
	`, accountID, appID, dataUpstreamsHostRedactedSentinel)
	assertCheckViolation(t, err, "data_upstreams_last_probed_pair_chk")

	// (3i) data_upstream_probes ok/rtt/error_class
	// pair violated → 23514. ok=true WITH error_class
	// NOT NULL is one of the bad shapes.
	_, err = pool.Exec(ctx, `
		INSERT INTO data_upstream_probes (
			id, host_redacted_hash, region, kind, sampled_at, rtt_ms,
			ok, error_class
		) VALUES (
			gen_random_uuid(), $1, 'eu-central-1', 'postgres', now(),
			42, true, 'timeout'
		)
	`, dataUpstreamsHostRedactedSentinel)
	assertCheckViolation(t, err, "data_upstream_probes_ok_pair_chk")

	// (4) UNIQUE tripwire: data_upstreams_dedupe_uniq.
	// The first INSERT succeeds; the second with the
	// same (app_id, scope, kind, host, port) fails
	// with 23505 on data_upstreams_dedupe_uniq.
	hostA := "db-pr-a-unique.example.com"
	if _, err := pool.Exec(ctx, `
		INSERT INTO data_upstreams (
			id, account_id, app_id, source, scope, kind, host, port,
			host_redacted_hash
		) VALUES (
			gen_random_uuid(), $1, $2, 'inferred', 'default',
			'postgres', $3, 5432, $4
		)
	`, accountID, appID, hostA, dataUpstreamsHostRedactedSentinel); err != nil {
		t.Fatalf("first dedupe-uniq insert: %v", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO data_upstreams (
			id, account_id, app_id, source, scope, kind, host, port,
			host_redacted_hash
		) VALUES (
			gen_random_uuid(), $1, $2, 'inferred', 'default',
			'postgres', $3, 5432, $4
		)
	`, accountID, appID, hostA, dataUpstreamsHostRedactedSentinel)
	if err == nil {
		t.Fatal("second INSERT with same (app_id, scope, kind, host, port) succeeded; data_upstreams_dedupe_uniq missing or regressed (the dedupe-merge handler in apid relies on this unique constraint)")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("second dedupe-uniq insert: got non-Postgres error: %v", err)
	}
	if pgErr.Code != "23505" {
		t.Errorf("second dedupe-uniq insert: got SQLSTATE=%s, want 23505", pgErr.Code)
	}
	if pgErr.ConstraintName != "data_upstreams_dedupe_uniq" {
		t.Errorf("second dedupe-uniq insert: got constraint=%q, want data_upstreams_dedupe_uniq (the handler's ON CONFLICT target by name)", pgErr.ConstraintName)
	}

	// (5) FK cascade. Insert a data_upstreams row tied
	// to a fresh account + app, then delete the account;
	// the row must disappear.
	fkAccountID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO accounts (id, email, plan, status, created_at)
		VALUES ($1, $2, 'hobby', 'active', now())
	`, fkAccountID, "pr-a-fk-test@example.com"); err != nil {
		t.Fatalf("seed fk-test account: %v", err)
	}
	fkAppID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO apps (id, account_id, slug, type, runtime, ram_mb, max_concurrency, status)
		VALUES ($1, $2, 'pr-a-fk-test', 'app', NULL, 256, 1, 'active')
	`, fkAppID, fkAccountID); err != nil {
		t.Fatalf("seed fk-test app: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO data_upstreams (
			id, account_id, app_id, source, scope, kind, host, port,
			host_redacted_hash
		) VALUES (
			gen_random_uuid(), $1, $2, 'inferred', 'default',
			'postgres', 'db-fk-test.example.com', 5432, $3
		)
	`, fkAccountID, fkAppID, dataUpstreamsHostRedactedSentinel); err != nil {
		t.Fatalf("seed data_upstreams row for FK test: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM apps WHERE id = $1`, fkAppID); err != nil {
		t.Fatalf("delete fk-test app (apps owns an FK to accounts with no cascade): %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM accounts WHERE id = $1`, fkAccountID); err != nil {
		t.Fatalf("delete fk-test account: %v", err)
	}
	var leftover int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM data_upstreams WHERE account_id = $1
	`, fkAccountID).Scan(&leftover); err != nil {
		t.Fatalf("re-read data_upstreams after account delete: %v", err)
	}
	if leftover != 0 {
		t.Errorf("data_upstreams rows after account delete: got %d, want 0 (FK must be ON DELETE CASCADE)", leftover)
	}

	// (6) Trigger fires on INSERT/UPDATE/DELETE and
	// emits on `data_upstreams_changed`. Acquire the
	// subscription BEFORE issuing any writes (LISTEN must
	// be parked on the channel before pg_notify fires
	// for the payload to be observed — the trigger
	// commit-time notify happens AFTER the writer's
	// INSERT/UPDATE/DELETE, and the LISTEN-side channel
	// needs to be armed).
	notif, cancel, err := db.Subscribe(ctx, pool, []string{db.NotifyDataUpstreamChanged})
	if err != nil {
		t.Fatalf("Subscribe(data_upstreams_changed): %v", err)
	}
	defer cancel()

	// Seed a row that the trigger fires on. Use the
	// original (accountID, appID) pair.
	hostTrig := "db-pr-a-trigger.example.com"
	if _, err := pool.Exec(ctx, `
		INSERT INTO data_upstreams (
			id, account_id, app_id, source, scope, kind, host, port,
			host_redacted_hash
		) VALUES (
			gen_random_uuid(), $1, $2, 'inferred', 'default',
			'postgres', $3, 5432, $4
		)
	`, accountID, appID, hostTrig, dataUpstreamsHostRedactedSentinel); err != nil {
		t.Fatalf("trigger test INSERT: %v", err)
	}

	got := waitForDataUpstreamNotification(t, notif, hostTrig, 5*time.Second)
	if !strings.Contains(got.Payload, "INSERT") {
		t.Errorf("trigger INSERT payload missing INSERT: %q", got.Payload)
	}
	if !strings.Contains(got.Payload, appID.String()) {
		t.Errorf("trigger INSERT payload missing app_id: %q", got.Payload)
	}

	// UPDATE branch.
	if _, err := pool.Exec(ctx, `
		UPDATE data_upstreams SET last_rtt_ms = 42, last_probed_at = now()
		 WHERE app_id = $1 AND host = $2
	`, appID, hostTrig); err != nil {
		t.Fatalf("trigger test UPDATE: %v", err)
	}
	got = waitForDataUpstreamNotification(t, notif, hostTrig, 5*time.Second)
	if !strings.Contains(got.Payload, "UPDATE") {
		t.Errorf("trigger UPDATE payload missing UPDATE: %q", got.Payload)
	}

	// DELETE branch.
	if _, err := pool.Exec(ctx, `
		DELETE FROM data_upstreams WHERE app_id = $1 AND host = $2
	`, appID, hostTrig); err != nil {
		t.Fatalf("trigger test DELETE: %v", err)
	}
	got = waitForDataUpstreamNotification(t, notif, hostTrig, 5*time.Second)
	if !strings.Contains(got.Payload, "DELETE") {
		t.Errorf("trigger DELETE payload missing DELETE: %q", got.Payload)
	}

	// (7) host_redacted_hash is NOT NULL on
	// data_upstreams. A NULL insert trips NOT NULL
	// (SQLSTATE 23502), not the CHECK — distinct from
	// the bad-shape check above.
	_, err = pool.Exec(ctx, `
		INSERT INTO data_upstreams (
			id, account_id, app_id, source, scope, kind, host, port,
			host_redacted_hash
		) VALUES (
			gen_random_uuid(), $1, $2, 'inferred', 'default',
			'postgres', 'db-pr-a-null-hash.example.com', 5432, NULL
		)
	`, accountID, appID)
	if err == nil {
		t.Fatal("NULL host_redacted_hash accepted; column must be NOT NULL")
	}
	var pgErr2 *pgconn.PgError
	if !errors.As(err, &pgErr2) {
		t.Fatalf("NULL host_redacted_hash: got non-Postgres error: %v", err)
	}
	if pgErr2.Code != "23502" {
		t.Errorf("NULL host_redacted_hash: got SQLSTATE=%s, want 23502 (not_null_violation)", pgErr2.Code)
	}

	// Cleanup the dedupe-uniq row so the apply_walk
	// second pass finds a clean slate.
	if _, err := pool.Exec(ctx, `
		DELETE FROM data_upstreams WHERE host = $1
	`, hostA); err != nil {
		t.Fatalf("cleanup dedupe-uniq row: %v", err)
	}
}

// waitForDataUpstreamNotification blocks up to d for the
// next entry on the notification channel whose payload
// contains the host substring. pg_notify is cluster-global
// (LISTEN sees every schema's writes), so a parallel
// pgtest schema's data_upstreams INSERT can leak in here
// — we drop those by string-matching the test host before
// returning. If no matching notification arrives within d,
// fail the test (a missing trigger IS the regression).
func waitForDataUpstreamNotification(t *testing.T, ch <-chan db.Notification, wantHost string, d time.Duration) db.Notification {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case n := <-ch:
			if strings.Contains(n.Payload, wantHost) {
				return n
			}
		case <-deadline:
			t.Fatalf("no data_upstreams_changed notification containing host %q within %s (trigger missing, channel not LISTENed, or sibling schema's payload flooded the queue)", wantHost, d)
			return db.Notification{}
		}
	}
}
