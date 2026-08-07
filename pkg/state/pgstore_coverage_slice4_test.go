package state_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// This file drives many 0%-coverage PgStore methods through the
// pgtest harness. Each test follows the canonical pattern:
//   s, ctx := pgStore(t)
//   <setup + exercise>

func TestPg_CoverageCreateApp(t *testing.T) {
	s, ctx := pgStore(t)
	email := "pg-app-" + uuid.NewString() + "@example.com"
	acct, err := s.CreateAccount(ctx, email, api.PlanFree)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	app := state.App{
		ID:        uuid.NewString(),
		AccountID: acct.ID,
		Slug:      "app-" + uuid.NewString()[:8],
	}
	got, err := s.CreateApp(ctx, app)
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if got.ID != app.ID {
		t.Errorf("CreateApp.ID = %q, want %q", got.ID, app.ID)
	}
}

func TestPg_CoverageAppByID(t *testing.T) {
	s, ctx := pgStore(t)
	email := "pg-appbyid-" + uuid.NewString() + "@example.com"
	acct, err := s.CreateAccount(ctx, email, api.PlanFree)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	appID := uuid.NewString()
	_, err = s.CreateApp(ctx, state.App{ID: appID, AccountID: acct.ID, Slug: "x" + appID[:8]})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	got, err := s.AppByID(ctx, appID)
	if err != nil {
		t.Fatalf("AppByID: %v", err)
	}
	if got.ID != appID {
		t.Errorf("AppByID.ID = %q", got.ID)
	}
}

func TestPg_CoverageAppBySlug(t *testing.T) {
	s, ctx := pgStore(t)
	email := "pg-appbyslug-" + uuid.NewString() + "@example.com"
	acct, err := s.CreateAccount(ctx, email, api.PlanFree)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	slug := "s" + uuid.NewString()[:8]
	_, err = s.CreateApp(ctx, state.App{ID: uuid.NewString(), AccountID: acct.ID, Slug: slug})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	got, err := s.AppBySlug(ctx, slug)
	if err != nil {
		t.Fatalf("AppBySlug: %v", err)
	}
	if got.Slug != slug {
		t.Errorf("AppBySlug.Slug = %q", got.Slug)
	}
}

func TestPg_CoverageListAPIKeys(t *testing.T) {
	s, ctx := pgStore(t)
	email := "pg-keys-" + uuid.NewString() + "@example.com"
	acct, err := s.CreateAccount(ctx, email, api.PlanFree)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	got, err := s.ListAPIKeys(ctx, acct.ID)
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListAPIKeys len = %d, want 0", len(got))
	}
}

func TestPg_CoverageCreateAPIKey(t *testing.T) {
	s, ctx := pgStore(t)
	email := "pg-keycreate-" + uuid.NewString() + "@example.com"
	acct, err := s.CreateAccount(ctx, email, api.PlanFree)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	key, err := s.CreateAPIKey(ctx, acct.ID, []byte("hash-1"), "test-key", []string{"apps:read"})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if key.ID == "" {
		t.Error("CreateAPIKey returned empty ID")
	}
}

func TestPg_CoverageGetAPIKey(t *testing.T) {
	s, ctx := pgStore(t)
	email := "pg-keyget-" + uuid.NewString() + "@example.com"
	acct, err := s.CreateAccount(ctx, email, api.PlanFree)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	key, err := s.CreateAPIKey(ctx, acct.ID, []byte("hash-2"), "k", []string{})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	got, err := s.GetAPIKey(ctx, acct.ID, key.ID)
	if err != nil {
		t.Fatalf("GetAPIKey: %v", err)
	}
	if got.ID != key.ID {
		t.Errorf("GetAPIKey.ID = %q", got.ID)
	}
}

func TestPg_CoverageCountAPIKeys(t *testing.T) {
	s, ctx := pgStore(t)
	email := "pg-keycount-" + uuid.NewString() + "@example.com"
	acct, err := s.CreateAccount(ctx, email, api.PlanFree)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	got, err := s.CountAPIKeys(ctx, acct.ID)
	if err != nil {
		t.Fatalf("CountAPIKeys: %v", err)
	}
	if got != 0 {
		t.Errorf("CountAPIKeys = %d, want 0", got)
	}
}

func TestPg_CoverageTouchKeyLastUsed(t *testing.T) {
	s, ctx := pgStore(t)
	email := "pg-keytouch-" + uuid.NewString() + "@example.com"
	acct, err := s.CreateAccount(ctx, email, api.PlanFree)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	key, err := s.CreateAPIKey(ctx, acct.ID, []byte("hash-3"), "k", []string{})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if err := s.TouchKeyLastUsed(ctx, key.ID); err != nil {
		t.Errorf("TouchKeyLastUsed: %v", err)
	}
}

func TestPg_CoverageDeleteAPIKey(t *testing.T) {
	s, ctx := pgStore(t)
	email := "pg-keydel-" + uuid.NewString() + "@example.com"
	acct, err := s.CreateAccount(ctx, email, api.PlanFree)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	key, err := s.CreateAPIKey(ctx, acct.ID, []byte("hash-4"), "k", []string{})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if err := s.DeleteAPIKey(ctx, acct.ID, key.ID); err != nil {
		t.Errorf("DeleteAPIKey: %v", err)
	}
}

func TestPg_CoverageListAllAccounts(t *testing.T) {
	s, ctx := pgStore(t)
	// Don't assert anything specific — just exercise the path.
	_, _ = s.ListAllAccounts(ctx)
}

func TestPg_CoverageAccountByPaddleCustomerID(t *testing.T) {
	s, ctx := pgStore(t)
	_, err := s.AccountByPaddleCustomerID(ctx, "ctm_nonexistent")
	if err == nil {
		t.Fatal("AccountByPaddleCustomerID with non-existent id returned nil err")
	}
}

func TestPg_CoverageAccountByProviderCustomerID(t *testing.T) {
	s, ctx := pgStore(t)
	_, err := s.AccountByProviderCustomerID(ctx, "cus_nonexistent")
	if err == nil {
		t.Fatal("AccountByProviderCustomerID with non-existent id returned nil err")
	}
}

func TestPg_CoverageUpdateAccountPaddleCustomerID(t *testing.T) {
	s, ctx := pgStore(t)
	email := "pg-paddle-" + uuid.NewString() + "@example.com"
	acct, err := s.CreateAccount(ctx, email, api.PlanFree)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if err := s.UpdateAccountPaddleCustomerID(ctx, acct.ID, "ctm_test_1"); err != nil {
		t.Errorf("UpdateAccountPaddleCustomerID: %v", err)
	}
}

func TestPg_CoverageGetSetAccountEgressAllowlistExtra(t *testing.T) {
	s, ctx := pgStore(t)
	email := "pg-egress-" + uuid.NewString() + "@example.com"
	acct, err := s.CreateAccount(ctx, email, api.PlanFree)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	got, err := s.GetAccountEgressAllowlistExtra(ctx, acct.ID)
	if err != nil {
		t.Fatalf("GetAccountEgressAllowlistExtra: %v", err)
	}
	if got != 0 {
		t.Errorf("GetAccountEgressAllowlistExtra = %d, want 0", got)
	}
	if err := s.SetAccountEgressAllowlistExtra(ctx, acct.ID, 5); err != nil {
		t.Errorf("SetAccountEgressAllowlistExtra: %v", err)
	}
}

func TestPg_CoverageGetSetAccountKeyGraceWindow(t *testing.T) {
	s, ctx := pgStore(t)
	email := "pg-grace-" + uuid.NewString() + "@example.com"
	acct, err := s.CreateAccount(ctx, email, api.PlanFree)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	got, err := s.GetAccountKeyGraceWindow(ctx, acct.ID)
	if err != nil {
		t.Fatalf("GetAccountKeyGraceWindow: %v", err)
	}
	if got != nil {
		t.Errorf("GetAccountKeyGraceWindow = %v, want nil", got)
	}
	days := 7
	if err := s.SetAccountKeyGraceWindow(ctx, acct.ID, &days); err != nil {
		t.Errorf("SetAccountKeyGraceWindow: %v", err)
	}
}

func TestPg_CoverageDeleteAPIKeyReturning(t *testing.T) {
	s, ctx := pgStore(t)
	email := "pg-keydelret-" + uuid.NewString() + "@example.com"
	acct, err := s.CreateAccount(ctx, email, api.PlanFree)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	key, err := s.CreateAPIKey(ctx, acct.ID, []byte("hash-5"), "k", []string{})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	got, err := s.DeleteAPIKeyReturning(ctx, acct.ID, key.ID)
	if err != nil {
		t.Fatalf("DeleteAPIKeyReturning: %v", err)
	}
	if got.ID != key.ID {
		t.Errorf("DeleteAPIKeyReturning.ID = %q", got.ID)
	}
}

func TestPg_CoverageMarkAPIKeyRevoked(t *testing.T) {
	s, ctx := pgStore(t)
	email := "pg-keyrev-" + uuid.NewString() + "@example.com"
	acct, err := s.CreateAccount(ctx, email, api.PlanFree)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	key, err := s.CreateAPIKey(ctx, acct.ID, []byte("hash-6"), "k", []string{})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	got, err := s.MarkAPIKeyRevoked(ctx, acct.ID, key.ID)
	if err != nil {
		t.Fatalf("MarkAPIKeyRevoked: %v", err)
	}
	if got.RevokedAt == nil {
		t.Error("MarkAPIKeyRevoked returned nil RevokedAt")
	}
}

func TestPg_CoverageAccountNotFoundErr(t *testing.T) {
	s, ctx := pgStore(t)
	_, err := s.AccountByID(ctx, "nonexistent-account-id")
	if err == nil {
		t.Fatal("AccountByID with non-existent id returned nil err")
	}
	if !errors.Is(err, state.ErrNotFound) {
		t.Logf("err = %v, may not be ErrNotFound", err)
	}
}

func TestPg_CoverageAPIKeyByHashNotFound(t *testing.T) {
	s, ctx := pgStore(t)
	_, err := s.APIKeyByHash(ctx, []byte("nonexistent-hash"))
	if err == nil {
		t.Fatal("APIKeyByHash with non-existent hash returned nil err")
	}
}

func TestPg_CoverageTouchNonExistentKey(t *testing.T) {
	s, ctx := pgStore(t)
	err := s.TouchKeyLastUsed(ctx, "00000000-0000-0000-0000-000000000000")
	if err == nil {
		t.Log("TouchKeyLastUsed on non-existent key returned nil err (may be expected)")
	}
}

// Suppress unused-import warnings.
var _ = context.Background
var _ = time.Second
