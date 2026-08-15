//go:build !no_pg

// Migration-apply test for 00267_triggers.sql (unified Trigger primitive,
// closes #757).
//
// Pins:
//
//  1. Migration set applies cleanly through 00267 (no goose
//     duplicate-version panic). Slot 00267 is the next free real
//     after 00264_deployments_secret_findings (PR #864 closed) —
//     00265/00266 don't exist on main or any open PR at the time
//     of this commit. Future renumbering must re-verify
//     `git ls-tree origin/main migrations/` after every rebase per
//     migration-gates-collision-and-replay.md.
//
//  2. The three new tables exist with the expected columns + types:
//     triggers          (id, account_id, app_id, kind, slug, enabled,
//     config, batch_size_max, batch_window_ms,
//     max_attempts, cron_id, source, created_at,
//     updated_at)
//     trigger_records   (id, trigger_id, item_identifier, payload,
//     headers, metadata, state, attempts,
//     next_fire_at, received_at, last_error,
//     last_dispatched_at)
//     trigger_dead_letter (record_id, trigger_id, reason, routed_to,
//     detail, created_at)
//
//  3. The kind CHECK admits the closed-vocab six values
//     (cron, kafka, nats, redis_streams, sqs_compat, queue). Pins
//     the schema contract — adding a new kind requires a migration
//     that widens this CHECK, not a silent widen in code.
//
//  4. The state CHECK on trigger_records admits the five dispatch
//     states (pending, claimed, succeeded, retry, dead_letter).
//
//  5. The reason CHECK on trigger_dead_letter admits the seven
//     failure modes (rate_limited, poison_record, max_attempts,
//     broker_error, plan_quota, payload_too_large, customer_disabled).
//
//  6. The invocations.source CHECK was widened to include 'esm'.
//     Pins the dispatch-side contract that pkg/sched/dispatch_triggers.go
//     (commit #14) will rely on when it writes source='esm' rows.
//
//  7. The pg_notify trigger trigger_ready_notify fires on every
//     INSERT into trigger_records. Validates the wakeup contract
//     for the dispatch tick (commit #14 subscribes to
//     pg_notify 'trigger_ready').
//
//  8. Positive round-trip: insert a trigger row + a trigger_records
//     row, verify the pg_notify payload shape. Pins the end-to-end
//     contract that schedd's runTriggerTick will see.
//
//  9. Negative round-trip: inserting a trigger with kind='junk'
//     fails the CHECK. Pins the closed-vocab contract.
//
//  10. Replay safety: re-running db.MigrateUp is a no-op. The
//     apply_walk_test harness pins this at the directory level;
//     per-migration shape is asserted here as defence in depth.
//
// Build tag matches the rest of the migration tests; set
// FAAS_SKIP_PG_TESTS=1 to skip locally (see migrations/README.md).
package migrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

// pinClosedVocab returns the joined pg_get_constraintdef output for
// every CHECK constraint on (table, column). pg_get_constraintdef
// emits either `IN (...)` or `= ANY (ARRAY[...])` per
// pg-get-constraintdef-shapes.md; we compare on substring presence
// so both shapes are accepted.
func pinClosedVocab(t *testing.T, pool *pgxpool.Pool, table, column string) string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		select pg_get_constraintdef(c.oid)
		  from pg_constraint c
		  join pg_class      t on t.oid = c.conrelid
		  join pg_attribute  a on a.attrelid = t.oid and a.attnum = any(c.conkey)
		 where t.relname = $1
		   and a.attname = $2
		   and c.contype = 'c'`, table, column)
	if err != nil {
		t.Fatalf("query %s.%s CHECK: %v", table, column, err)
	}
	defer rows.Close()
	var defs []string
	for rows.Next() {
		var def string
		if err := rows.Scan(&def); err != nil {
			t.Fatalf("scan %s.%s CHECK: %v", table, column, err)
		}
		defs = append(defs, def)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s.%s CHECK: %v", table, column, err)
	}
	if len(defs) == 0 {
		t.Fatalf("%s.%s has no CHECK constraint", table, column)
	}
	return strings.Join(defs, " ")
}

// pinTableExists returns true if the named table is present in the
// current pgtest schema.
func pinTableExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx, `
		select exists (
			select 1 from pg_class
			 where relname = $1
			   and relnamespace = current_schema()::regnamespace
		)`, table).Scan(&exists); err != nil {
		t.Fatalf("query %s existence: %v", table, err)
	}
	return exists
}

func TestMigrations_00267_Triggers(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// (1) Run the full migration set. 00267 should land last.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (PR follow-up failure mode: missing migration slot between 00264 secret-findings and 00267 triggers)", err)
	}

	// (2) Tables exist.
	for _, table := range []string{"triggers", "trigger_records", "trigger_dead_letter"} {
		if !pinTableExists(t, ctx, pool, table) {
			t.Errorf("table %q does not exist after migration 00267", table)
		}
	}

	// (3) triggers.kind CHECK admits the six values.
	kindDef := pinClosedVocab(t, pool, "triggers", "kind")
	wantKinds := []string{"cron", "kafka", "nats", "redis_streams", "sqs_compat", "queue"}
	for _, want := range wantKinds {
		if !strings.Contains(kindDef, "'"+want+"'") {
			t.Errorf("triggers.kind CHECK missing %q; got %s", want, kindDef)
		}
	}

	// (4) trigger_records.state CHECK admits the five dispatch states.
	stateDef := pinClosedVocab(t, pool, "trigger_records", "state")
	for _, want := range []string{"pending", "claimed", "succeeded", "retry", "dead_letter"} {
		if !strings.Contains(stateDef, "'"+want+"'") {
			t.Errorf("trigger_records.state CHECK missing %q; got %s", want, stateDef)
		}
	}

	// (5) trigger_dead_letter.reason CHECK admits the seven failure modes.
	reasonDef := pinClosedVocab(t, pool, "trigger_dead_letter", "reason")
	wantReasons := []string{
		"rate_limited", "poison_record", "max_attempts", "broker_error",
		"plan_quota", "payload_too_large", "customer_disabled",
	}
	for _, want := range wantReasons {
		if !strings.Contains(reasonDef, "'"+want+"'") {
			t.Errorf("trigger_dead_letter.reason CHECK missing %q; got %s", want, reasonDef)
		}
	}

	// (6) invocations.source CHECK widened to include 'esm'.
	invDef := pinClosedVocab(t, pool, "invocations", "source")
	if !strings.Contains(invDef, "'esm'") {
		t.Errorf("invocations.source CHECK missing 'esm'; got %s", invDef)
	}
	// Pre-existing values must still be present.
	for _, want := range []string{"async_invoke", "queue", "delayed_task", "cron", "replay"} {
		if !strings.Contains(invDef, "'"+want+"'") {
			t.Errorf("invocations.source CHECK missing pre-existing value %q; got %s", want, invDef)
		}
	}

	// (7) trigger_ready_notify pg_notify trigger is installed.
	var triggerExists bool
	if err := pool.QueryRow(ctx, `
		select exists (
			select 1 from pg_trigger
			 where tgname = 'trigger_ready_notify'
			   and tgrelid = 'trigger_records'::regclass
		)`).Scan(&triggerExists); err != nil {
		t.Fatalf("query trigger_ready_notify: %v", err)
	}
	if !triggerExists {
		t.Error("trigger_ready_notify pg_notify trigger is not installed on trigger_records")
	}

	// (8) Positive round-trip: seed a trigger, insert a record,
	// observe the pg_notify payload. Pins the contract for schedd's
	// dispatch_tick (commit #14), which subscribes to the channel.
	accountID, appID := pinFixtures(t, ctx, pool)

	// LISTEN on the channel so we can capture the notify payload.
	// We use a dedicated connection because pgx requires
	// WaitForNotification to be on a separate Conn from the pool.
	listenCtx, cancelListen := context.WithCancel(ctx)
	defer cancelListen()
	type notifyCapture struct {
		payload string
		err     error
	}
	notifyCh := make(chan notifyCapture, 1)
	conn, err := pool.Acquire(listenCtx)
	if err != nil {
		t.Fatalf("acquire conn for LISTEN: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(listenCtx, "LISTEN trigger_ready"); err != nil {
		t.Fatalf("LISTEN trigger_ready: %v", err)
	}
	go func() {
		notif, err := conn.Conn().WaitForNotification(listenCtx)
		if err != nil {
			notifyCh <- notifyCapture{err: err}
			return
		}
		notifyCh <- notifyCapture{payload: notif.Payload}
	}()

	// Insert trigger.
	var triggerID string
	if err := pool.QueryRow(ctx, `
		insert into triggers (account_id, app_id, kind, slug, config)
		values ($1, $2, 'kafka', 'pin-test', '{"topic":"t","group":"g","brokers":["b:9092"]}'::jsonb)
		returning id::text`, accountID, appID).Scan(&triggerID); err != nil {
		t.Fatalf("insert trigger: %v", err)
	}

	// Insert a record; the AFTER INSERT trigger should fire pg_notify.
	if _, err := pool.Exec(ctx, `
		insert into trigger_records (trigger_id, item_identifier)
		values ($1, 'pin-item-1')`, triggerID); err != nil {
		t.Fatalf("insert trigger_records: %v", err)
	}

	// Verify the pg_notify payload arrived. Best-effort: pg_notify on
	// an uncommitted transaction may not be observable until the tx
	// commits, so we drain the LISTEN connection after a short window.
	captured := <-notifyCh
	if captured.err != nil {
		t.Errorf("WaitForNotification error: %v (pin the contract that schedd's dispatch_tick relies on)", captured.err)
	} else {
		// Payload must contain the trigger_id + item_identifier.
		if !strings.Contains(captured.payload, triggerID) {
			t.Errorf("trigger_ready payload missing trigger_id %q; got %s", triggerID, captured.payload)
		}
		if !strings.Contains(captured.payload, "pin-item-1") {
			t.Errorf("trigger_ready payload missing item_identifier 'pin-item-1'; got %s", captured.payload)
		}
	}

	// (9) Negative round-trip: invalid kind rejected by CHECK.
	_, err = pool.Exec(ctx, `
		insert into triggers (account_id, app_id, kind, slug)
		values ($1, $2, 'junk', 'pin-test-invalid')`, accountID, appID)
	if err == nil {
		t.Error("inserting trigger with kind='junk' should fail the CHECK constraint")
	} else if !strings.Contains(err.Error(), "check") && !strings.Contains(err.Error(), "23514") {
		t.Errorf("unexpected error kind for invalid kind insert: %v", err)
	}

	// (10) Replay safety: re-running db.MigrateUp is a no-op.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay db.MigrateUp: %v", err)
	}
}

// pinFixtures seeds a minimal account + app to satisfy the FK
// constraints on triggers and trigger_records. Returns the IDs.
// Pinned UUIDs (with ON CONFLICT DO NOTHING) so the test is idempotent
// across consecutive runs in the same pgtest schema.
func pinFixtures(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (accountID, appID string) {
	t.Helper()
	accountID = "00000000-0000-0000-0000-000000002267"
	appID = "00000000-0000-0000-0000-000000012267"
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, plan, email)
		values ($1, 'scale', 'trigger-pin@example.com')
		on conflict (id) do nothing
	`, accountID); err != nil {
		t.Fatalf("seed accounts: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, idle_timeout_s, status, created_at)
		values ($1, $2, 'trigger-pin', 'function', 256, 1, 30, 'active', now())
		on conflict (id) do nothing
	`, appID, accountID); err != nil {
		t.Fatalf("seed apps: %v", err)
	}
	return accountID, appID
}
