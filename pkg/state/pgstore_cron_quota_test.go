package state_test

// PR #340 follow-up: round-trip CreateCronIfUnderQuota against a real
// Postgres cluster so a typo in the tx body, an off-by-one on the
// count, or a Scope mis-wire can't ship silently. The HTTP tests in
// cmd/apid/handlers_ext_test.go run against the in-process MemStore;
// this file is the only thing that proves the PgStore predicate
// matches the MemStore one byte-for-byte.
//
// Uses pgtest.Open so the whole file skips cleanly when Postgres is
// unreachable (same pattern as pgstore_account_deletion_test.go).

import (
	"errors"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// TestPgStore_CreateCronIfUnderQuota_PerAppCap fills one app to its
// per-app cron limit (Pro: 20) and asserts the next insert returns
// *state.CronQuotaError with Scope=App. The hand-written SQL in
// pgstore.go is the only thing exercising this — losing the partial
// index or the count predicate would silently raise the cap.
func TestPgStore_CreateCronIfUnderQuota_PerAppCap(t *testing.T) {
	s, ctx := pgStore(t)
	limits := api.MustLimitsFor(api.PlanPro) // 20/app, 50/acct
	acct, app, _ := seedLiveDeploy(t, s, ctx)
	for i := 0; i < limits.CronLimitPerApp; i++ {
		if _, err := s.CreateCron(ctx, app, "*/5 * * * *", "/x", true); err != nil {
			t.Fatalf("seed cron %d: %v", i, err)
		}
	}
	_, err := s.CreateCronIfUnderQuota(ctx, app, "*/5 * * * *", "/x", true, limits)
	if err == nil {
		t.Fatal("expected *CronQuotaError at per-app cap, got nil")
	}
	var qe *state.CronQuotaError
	if !errors.As(err, &qe) {
		t.Fatalf("expected *CronQuotaError, got %T: %v", err, err)
	}
	if qe.Scope != state.CronQuotaScopeApp {
		t.Errorf("scope = %q, want %q", qe.Scope, state.CronQuotaScopeApp)
	}
	if qe.Limit != limits.CronLimitPerApp {
		t.Errorf("limit = %d, want %d", qe.Limit, limits.CronLimitPerApp)
	}
	if qe.Observed != limits.CronLimitPerApp {
		t.Errorf("observed = %d, want %d", qe.Observed, limits.CronLimitPerApp)
	}
	// errors.Is must match the sentinel — handlers depend on this.
	if !errors.Is(err, state.ErrCronQuotaExceeded) {
		t.Error("errors.Is(err, ErrCronQuotaExceeded) = false, want true")
	}
	_ = acct // suppress unused
}

// TestPgStore_CreateCronIfUnderQuota_PerAccountCap seeds crons across
// three apps on the same account so the next insert on the third app
// lands at the per-account cap (Pro: 50) with per-app still under.
// Proves the join through apps and the FOR UPDATE on the apps row
// together let the per-account count fire before the per-app cap.
func TestPgStore_CreateCronIfUnderQuota_PerAccountCap(t *testing.T) {
	s, ctx := pgStore(t)
	limits := api.MustLimitsFor(api.PlanPro) // 20/app, 50/acct
	acct, _, _ := seedLiveDeploy(t, s, ctx)

	// Two more apps on the same account. Apps carry CHECK
	// constraints (apps_max_concurrency_check, etc.) so we set the
	// same shape as seedLiveDeploy's App row.
	appARec, err := s.CreateApp(ctx, state.App{
		AccountID: acct, Slug: "cron-acct-a", Type: state.AppTypeFunction,
		RAMMB: 512, MaxConcurrency: 5, IdleTimeoutS: 60,
	})
	if err != nil {
		t.Fatalf("CreateApp A: %v", err)
	}
	appBRec, err := s.CreateApp(ctx, state.App{
		AccountID: acct, Slug: "cron-acct-b", Type: state.AppTypeFunction,
		RAMMB: 512, MaxConcurrency: 5, IdleTimeoutS: 60,
	})
	if err != nil {
		t.Fatalf("CreateApp B: %v", err)
	}
	appA, appB := appARec.ID, appBRec.ID
	for i := 0; i < limits.CronLimitPerApp-1; i++ {
		if _, err := s.CreateCron(ctx, appA, "*/5 * * * *", "/x", true); err != nil {
			t.Fatalf("seed appA %d: %v", i, err)
		}
	}
	for i := 0; i < limits.CronLimitPerApp-1; i++ {
		if _, err := s.CreateCron(ctx, appB, "*/5 * * * *", "/x", true); err != nil {
			t.Fatalf("seed appB %d: %v", i, err)
		}
	}
	fillC := limits.CronLimitPerAccount - 2*(limits.CronLimitPerApp-1)
	for i := 0; i < fillC; i++ {
		if _, err := s.CreateCron(ctx, appB, "*/5 * * * *", "/x", true); err != nil {
			t.Fatalf("seed appB tail %d: %v", i, err)
		}
	}
	// Now per-account is at the cap. Next insert via the *quota*
	// method on a third app must trip the account arm.
	appCRec, err := s.CreateApp(ctx, state.App{
		AccountID: acct, Slug: "cron-acct-c", Type: state.AppTypeFunction,
		RAMMB: 512, MaxConcurrency: 5, IdleTimeoutS: 60,
	})
	if err != nil {
		t.Fatalf("CreateApp C: %v", err)
	}
	appC := appCRec.ID
	_, err = s.CreateCronIfUnderQuota(ctx, appC, "*/5 * * * *", "/x", true, limits)
	if err == nil {
		t.Fatal("expected *CronQuotaError at per-account cap, got nil")
	}
	var qe *state.CronQuotaError
	if !errors.As(err, &qe) {
		t.Fatalf("expected *CronQuotaError, got %T: %v", err, err)
	}
	if qe.Scope != state.CronQuotaScopeAccount {
		t.Errorf("scope = %q, want %q (per-account is at cap; per-app on appC is 0/20)", qe.Scope, state.CronQuotaScopeAccount)
	}
	if qe.Limit != limits.CronLimitPerAccount {
		t.Errorf("limit = %d, want %d", qe.Limit, limits.CronLimitPerAccount)
	}
	if qe.Observed != limits.CronLimitPerAccount {
		t.Errorf("observed = %d, want %d", qe.Observed, limits.CronLimitPerAccount)
	}
}

// TestPgStore_CreateCronIfUnderQuota_AppDeletedReturnsNotFound guards
// the failure mode the doc comment calls out: a soft-deleted app must
// not be cron-creatable. Mirrors MemStore's predicate (status ==
// AppDeleted → ErrNotFound).
func TestPgStore_CreateCronIfUnderQuota_AppDeletedReturnsNotFound(t *testing.T) {
	s, ctx := pgStore(t)
	limits := api.MustLimitsFor(api.PlanPro)
	_, app, _ := seedLiveDeploy(t, s, ctx)
	deleted := state.AppDeleted
	if _, err := s.UpdateApp(ctx, app, state.UpdateAppParams{Status: &deleted}); err != nil {
		t.Fatalf("UpdateApp Status=AppDeleted: %v", err)
	}
	_, err := s.CreateCronIfUnderQuota(ctx, app, "*/5 * * * *", "/x", true, limits)
	if !errors.Is(err, state.ErrNotFound) {
		t.Errorf("expected ErrNotFound for soft-deleted app, got %v", err)
	}
}
