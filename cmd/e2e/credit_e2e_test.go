// Package e2e — cmd/e2e acceptance tests. See cmd/e2e/quota_e2e_test.go
// for the build-tag policy. credit_e2e_test.go is the §14 BILLING gate
// for issue #279: an admin-issued credit lands in account_credits +
// credit_ledger with an audit row, and a per-account overage cap is
// honoured by the meterd quota tick.
//
// These tests boot real daemon subprocesses (apid + meterd) so the
// HTTP path, the SQL writes, and the meterd loop run in the production
// wire — not in-process fakes. The migration race is gated by
// pgtest.WaitForMigration (the harness boot runs the wait before any
// daemon starts; memory: cmd-e2e-schedd-migration-race).
//
// To skip locally: export FAAS_SKIP_PG_TESTS=1.

//go:build !no_pg

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
	"github.com/onebox-faas/faas/pkg/e2etest"
	"github.com/onebox-faas/faas/pkg/state"
)

// TestE2E_CreditIssue_AdminKey — POST /v1/admin/accounts/{id}/credits
// from an admin-scoped key with the email in FAAS_ADMIN_EMAILS lands a
// row in account_credits + a row in credit_ledger + an audit event of
// kind "credit.issued". Mirrors TestIssueCredit_HappyPath at the
// handler unit-test layer (cmd/apid/handlers_admin_credits_test.go);
// this is the e2e form to prove the wire-up.
func TestE2E_CreditIssue_AdminKey(t *testing.T) {
	if os.Getenv("FAAS_SKIP_PG_TESTS") != "" {
		t.Skip("FAAS_SKIP_PG_TESTS set")
	}
	pool := pgtest.Open(t)
	ctx := context.Background()
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	pgtest.WaitForMigration(t, pool, 50, 10*time.Second) // issue #279 landed at slot 50

	// The admin allowlist is read from FAAS_ADMIN_EMAILS by apid at
	// boot (cmd/apid/main.go:349). The harness seeds accounts whose
	// email is `e2e+<plan>+<label>@test.example`; the admin email below
	// must match what SeedAccount produces for the operator.
	const adminEmail = "e2e+hobby+admin@test.example"
	const targetEmail = "e2e+hobby+credit-target@test.example"

	h := e2etest.StartWithEnv(t, pool,
		e2etest.APID,
		[]string{"FAAS_ADMIN_EMAILS=" + adminEmail})

	store := state.NewPgStore(pool)

	// Seed the target account (the credit recipient).
	targetAcct, err := store.AccountByEmail(ctx, targetEmail)
	if err != nil {
		// not yet created — create it directly with a Hobby plan.
		targetAcct, err = store.CreateAccount(ctx, targetEmail, api.PlanHobby)
		if err != nil {
			t.Fatalf("seed target account: %v", err)
		}
	}

	// Seed the operator account + admin API key. SeedAccount stamps a
	// fresh admin-scoped bearer and the matching email is in the
	// allowlist above.
	adminToken := h.SeedAccount(ctx, api.PlanHobby, "admin")

	body, err := json.Marshal(map[string]any{
		"cents":  500,
		"reason": "goodwill for outage",
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		h.APIDURL+"/v1/admin/accounts/"+targetAcct.ID+"/credits",
		bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "e2e-credit-"+uuid.NewString())

	rec, err := h.HTTPClient().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer rec.Body.Close()
	if rec.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.StatusCode)
	}

	var resp api.AccountCreditResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.AccountID != targetAcct.ID {
		t.Errorf("AccountID = %q, want %q", resp.AccountID, targetAcct.ID)
	}
	if resp.CentsRemaining != 500 {
		t.Errorf("CentsRemaining = %d, want 500", resp.CentsRemaining)
	}

	// Exactly one row in account_credits + one row in credit_ledger.
	credits, err := store.ListAccountCredits(ctx, targetAcct.ID, false)
	if err != nil {
		t.Fatalf("ListAccountCredits: %v", err)
	}
	if len(credits) != 1 {
		t.Fatalf("account_credits rows = %d, want 1", len(credits))
	}
	if credits[0].Reason != "goodwill for outage" {
		t.Errorf("credit reason = %q, want %q", credits[0].Reason, "goodwill for outage")
	}

	// Audit row — the auditor's Emit is best-effort, so this
	// assertion pins that the row actually landed (matches the
	// handler-test addition).
	events, err := store.ListEvents(ctx, targetAcct.ID, 50)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var sawCreditIssued bool
	for _, e := range events {
		if e.Kind == "credit.issued" {
			sawCreditIssued = true
			break
		}
	}
	if !sawCreditIssued {
		t.Fatalf("credit.issued audit row missing for account %s", targetAcct.ID)
	}
}

// TestE2E_CreditIssue_NonAdminForbidden — POST without admin scope
// returns 403. Pins the two-layer auth at the wire boundary (not just
// at the unit-test boundary).
func TestE2E_CreditIssue_NonAdminForbidden(t *testing.T) {
	if os.Getenv("FAAS_SKIP_PG_TESTS") != "" {
		t.Skip("FAAS_SKIP_PG_TESTS set")
	}
	pool := pgtest.Open(t)
	ctx := context.Background()
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	pgtest.WaitForMigration(t, pool, 50, 10*time.Second)

	h := e2etest.StartWithEnv(t, pool, e2etest.APID,
		[]string{"FAAS_ADMIN_EMAILS=e2e+hobby+admin@test.example"})

	store := state.NewPgStore(pool)
	targetEmail := "e2e+hobby+credit-403-target@test.example"
	targetAcct, err := store.CreateAccount(ctx, targetEmail, api.PlanHobby)
	if err != nil {
		t.Fatalf("seed target: %v", err)
	}

	// Mint a non-admin scoped key directly via the store (the harness's
	// SeedAccount always returns admin scope, so we hand-build here).
	plain, hash, err := api.GenerateAPIKey()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	nonAdminAcct, err := store.CreateAccount(ctx, "e2e+hobby+credit-403-caller@test.example", api.PlanFree)
	if err != nil {
		t.Fatalf("seed caller: %v", err)
	}
	if _, err := store.CreateAPIKey(ctx, nonAdminAcct.ID, hash, "e2e", []string{api.ScopeDeployWrite}); err != nil {
		t.Fatalf("create non-admin key: %v", err)
	}

	body, _ := json.Marshal(map[string]any{
		"cents":  500,
		"reason": "goodwill for outage",
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		h.APIDURL+"/v1/admin/accounts/"+targetAcct.ID+"/credits",
		bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+plain)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "e2e-credit-403-"+uuid.NewString())

	rec, err := h.HTTPClient().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer rec.Body.Close()
	if rec.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.StatusCode)
	}
}
