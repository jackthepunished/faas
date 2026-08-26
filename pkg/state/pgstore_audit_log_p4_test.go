// Operator-side observability mega-PR — Commit 6 (P4 audit filter
// extensions). pgstore parity tests for the three new WHERE clauses
// added to ListAuditLog in pkg/state/pgstore.go:17400-17586:
//
//	?actor_email=<email>          → exact match on account_email
//	?target_account_id=<uuid>     → JSONB containment on data
//	                              (data @> jsonb_build_object('target_account_id', $N))
//	?operator_only=true           → kind LIKE 'operator.action.%' sugar
//	                              (defensive — handler enforces
//	                              mutual exclusivity with kind_prefix)
//
// Pattern follows pkg/state/pgstore_account_credits_test.go. Each
// test gates behind DATABASE_URL via pgtest.Open.
//
// JSONB containment is exercised end-to-end (PgStore round-trip;
// pgx's JSON driver handles the marshal). The MemStore parity lives
// in handlers_admin_obs_pr3_p4_test.go via the in-memory store
// branches, which is sufficient for the handler projection.

package state_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
	"github.com/onebox-faas/faas/pkg/state"
)

// pgStoreAuditLogP4WithPool mirrors pgStoreAccountCreditsWithPool
// — returns the store + the underlying pool so a test can plant
// audit_log fixtures directly (no emit-helper is exercised; the
// tests only need the read-side WHERE-clause paths).
func pgStoreAuditLogP4WithPool(t *testing.T) (*state.PgStore, *pgxpool.Pool, context.Context) {
	t.Helper()
	pool := pgtest.Open(t)
	ctx := context.Background()
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return state.NewPgStore(pool), pool, ctx
}

// plantAuditLog inserts a one-row audit_log fixture directly so the
// test can decouple the read-side WHERE clauses from any emit path
// (Commit 3's emit helpers are exercised separately in
// cmd/apid/handlers_admin_force_*_test.go).
func plantAuditLog(t *testing.T, pool *pgxpool.Pool, ctx context.Context, entry state.AuditLog) {
	t.Helper()
	if entry.ID == uuid.Nil {
		entry.ID = uuid.New()
	}
	if entry.ReceivedAt.IsZero() {
		entry.ReceivedAt = time.Now().UTC()
	}
	if _, err := pool.Exec(ctx,
		`insert into audit_log
		    (id, kind, account_id, account_email, actor, received_at, data)
		 values ($1, $2, $3, $4, $5, $6, $7::jsonb)`,
		entry.ID,
		entry.Kind,
		entry.AccountID,
		entry.AccountEmail,
		entry.Actor,
		entry.ReceivedAt,
		[]byte(entry.Data),
	); err != nil {
		t.Fatalf("plant audit_log: %v", err)
	}
}

func TestPgStoreListAuditLog_ActorEmail_MatchesByCapturedEmail(t *testing.T) {
	store, pool, ctx := pgStoreAuditLogP4WithPool(t)
	acct, err := store.CreateAccount(ctx, "alice@example.com", api.PlanHobby)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	acctID, err := uuid.Parse(acct.ID)
	if err != nil {
		t.Fatalf("parse account id: %v", err)
	}
	plantAuditLog(t, pool, ctx, state.AuditLog{
		Kind:         "operator.action.view",
		AccountID:    &acctID,
		AccountEmail: "alice@example.com",
		Actor:        "ops@faas.local",
		Data:         json.RawMessage(`{"endpoint":"/v1/admin/apps/foo/metrics"}`),
	})
	plantAuditLog(t, pool, ctx, state.AuditLog{
		Kind:         "operator.action.view",
		AccountID:    &acctID,
		AccountEmail: "alice@example.com",
		Actor:        "ops@faas.local",
		Data:         json.RawMessage(`{"endpoint":"/v1/admin/apps/foo/wake-timeline"}`),
	})
	plantAuditLog(t, pool, ctx, state.AuditLog{
		Kind:         "audit.account.deleted",
		AccountEmail: "bob@example.com",
		Actor:        "ops@faas.local",
	})

	matched, err := store.ListAuditLog(ctx, state.AuditLogFilter{
		ActorEmail: ptrString("alice@example.com"),
		Limit:      100,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(matched) != 2 {
		t.Fatalf("len = %d, want 2 (alice's two operator.action.view rows)", len(matched))
	}
	for _, row := range matched {
		if row.AccountEmail != "alice@example.com" {
			t.Fatalf("account_email = %q, want alice@example.com", row.AccountEmail)
		}
	}
}

func TestPgStoreListAuditLog_ActorEmail_NoMatchReturnsEmpty(t *testing.T) {
	store, pool, ctx := pgStoreAuditLogP4WithPool(t)
	plantedEmail := "carol@example.com"
	plantedAcct, err := store.CreateAccount(ctx, plantedEmail, api.PlanHobby)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	plantedAcctID, err := uuid.Parse(plantedAcct.ID)
	if err != nil {
		t.Fatalf("parse account id: %v", err)
	}
	plantAuditLog(t, pool, ctx, state.AuditLog{
		Kind:         "operator.action.view",
		AccountID:    &plantedAcctID,
		AccountEmail: plantedEmail,
		Actor:        "ops@faas.local",
	})

	matched, err := store.ListAuditLog(ctx, state.AuditLogFilter{
		ActorEmail: ptrString("nobody@example.com"),
		Limit:      100,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(matched) != 0 {
		t.Fatalf("len = %d, want 0 (no match for nobody@example.com)", len(matched))
	}
}

func TestPgStoreListAuditLog_TargetAccountID_JSONBContainment(t *testing.T) {
	store, pool, ctx := pgStoreAuditLogP4WithPool(t)
	targetAcct, err := store.CreateAccount(ctx, "target@example.com", api.PlanHobby)
	if err != nil {
		t.Fatalf("create target account: %v", err)
	}
	targetID := targetAcct.ID

	// Row written by operator.action.park_instance — sets
	// data->>'target_account_id' = <target.uuid> (the post-commit-3
	// emit shape; verified by handlers_admin_force_park_test.go).
	plantAuditLog(t, pool, ctx, state.AuditLog{
		Kind:         "operator.action.park_instance",
		AccountEmail: "ops@faas.local",
		Actor:        "ops@faas.local",
		Data:         json.RawMessage(`{"target_account_id":"` + targetID + `","instance_id":"abc"}`),
	})
	plantAuditLog(t, pool, ctx, state.AuditLog{
		Kind:         "operator.action.view",
		AccountEmail: "ops@faas.local",
		Actor:        "ops@faas.local",
		Data:         json.RawMessage(`{"target_account_id":"` + targetID + `","endpoint":"/v1/admin/apps/foo/metrics"}`),
	})
	// Decoy row: same kind but different target.
	plantAuditLog(t, pool, ctx, state.AuditLog{
		Kind:         "operator.action.view",
		AccountEmail: "ops@faas.local",
		Actor:        "ops@faas.local",
		Data:         json.RawMessage(`{"target_account_id":"00000000-0000-0000-0000-000000000000","endpoint":"/v1/admin/apps/bar/metrics"}`),
	})

	matched, err := store.ListAuditLog(ctx, state.AuditLogFilter{
		TargetAccountID:  &targetID,
		IncludeAnonymous: true,
		Limit:            100,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(matched) != 2 {
		t.Fatalf("len = %d, want 2 (target-account rows only)", len(matched))
	}
	for _, row := range matched {
		// Each row's data must actually contain the target id —
		// guards against the JSONB containment regex being
		// substituted with a substring-of-data scan.
		if !dataContains(row.Data, targetID) {
			t.Fatalf("row %s data %s missing target id %s", row.ID, row.Data, targetID)
		}
	}
}

func TestPgStoreListAuditLog_OperatorOnly_SortsToOperatorActionFamilyOnly(t *testing.T) {
	store, pool, ctx := pgStoreAuditLogP4WithPool(t)
	acct, err := store.CreateAccount(ctx, "ops@example.com", api.PlanHobby)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	acctID, err := uuid.Parse(acct.ID)
	if err != nil {
		t.Fatalf("parse account id: %v", err)
	}

	plantAuditLog(t, pool, ctx, state.AuditLog{
		Kind: "operator.action.view", AccountID: &acctID, AccountEmail: "ops@example.com", Actor: "ops@faas.local",
		Data: json.RawMessage(`{}`),
	})
	plantAuditLog(t, pool, ctx, state.AuditLog{
		Kind: "operator.action.park_instance", AccountID: &acctID, AccountEmail: "ops@example.com", Actor: "ops@faas.local",
		Data: json.RawMessage(`{}`),
	})
	plantAuditLog(t, pool, ctx, state.AuditLog{
		Kind: "operator.action.force_cold_boot", AccountID: &acctID, AccountEmail: "ops@example.com", Actor: "ops@faas.local",
		Data: json.RawMessage(`{}`),
	})
	plantAuditLog(t, pool, ctx, state.AuditLog{
		Kind: "audit.account.deleted", AccountID: &acctID, AccountEmail: "ops@example.com",
	})
	plantAuditLog(t, pool, ctx, state.AuditLog{
		Kind: "pii.accessed", AccountID: &acctID, AccountEmail: "ops@example.com", Actor: "ops@faas.local",
		Data: json.RawMessage(`{}`),
	})

	matched, err := store.ListAuditLog(ctx, state.AuditLogFilter{
		OperatorOnly: true,
		Limit:        100,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(matched) != 3 {
		t.Fatalf("len = %d, want 3 (three operator.action.* rows)", len(matched))
	}
	for _, row := range matched {
		if got := row.Kind[:len("operator.action.")]; got != "operator.action." {
			t.Fatalf("kind %q does not have operator.action. prefix", row.Kind)
		}
	}
}

func TestPgStoreListAuditLog_OperatorOnlyForcesPrefixOverKindPrefix(t *testing.T) {
	// Defensive SQL-layer guard: the handler returns 400 when both
	// are set, but if a future caller forgets, the SQL ignores the
	// KindPrefix in favor of the operator.action. prefix. We don't
	// care to test the handler here — that's done in the handler
	// test file (handlers_admin_obs_pr3_p4_test.go). At the SQL
	// layer we simply verify the rows returned are operator rows
	// when OperatorOnly is true.
	store, pool, ctx := pgStoreAuditLogP4WithPool(t)
	acct, err := store.CreateAccount(ctx, "ops@example.com", api.PlanHobby)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	acctID, err := uuid.Parse(acct.ID)
	if err != nil {
		t.Fatalf("parse account id: %v", err)
	}
	plantAuditLog(t, pool, ctx, state.AuditLog{
		Kind: "operator.action.view", AccountID: &acctID, AccountEmail: "ops@example.com",
		Data: json.RawMessage(`{}`),
	})
	plantAuditLog(t, pool, ctx, state.AuditLog{
		Kind: "audit.account.deleted", AccountID: &acctID, AccountEmail: "ops@example.com",
	})

	matched, err := store.ListAuditLog(ctx, state.AuditLogFilter{
		KindPrefix:   "audit.account.", // what the handler MUST refuse — proven here
		OperatorOnly: true,
		Limit:        100,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(matched) != 1 {
		t.Fatalf("len = %d, want 1 (operator row only; the stray KindPrefix is ignored)", len(matched))
	}
	if matched[0].Kind != "operator.action.view" {
		t.Fatalf("kind = %q, want operator.action.view", matched[0].Kind)
	}
}

func ptrString(s string) *string { return &s }

// dataContains is a tiny helper — avoids a strings-import dance
// just to check that the projected data still contains the
// target UUID after the JSONB round-trip.
func dataContains(data json.RawMessage, needle string) bool {
	if len(data) == 0 {
		return false
	}
	for i := 0; i+len(needle) <= len(data); i++ {
		if string(data[i:i+len(needle)]) == needle {
			return true
		}
	}
	return false
}
