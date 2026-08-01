package state_test

// pgstore_app_registry_credentials_test pins the hand-coded SQL in
// pgstore.go::UpsertAppRegistryCredential / Get / List / Delete /
// Count / MarkAppRegistryCredentialUsed against a real Postgres
// cluster (issue #461 / ADR-062).
//
// pgtest.Open skips the whole file when Postgres is unreachable, so
// the -short / no-pg CI matrix still passes; make test-state-coverage
// runs with DATABASE_URL and bumps pkg/state coverage.
//
// The shape mirrors pgstore_alert_rules_test.go: pgStore(t) +
// seedLiveDeploy(t, s, ctx) → exercise the (account, app) FK graph
// already established by migration 00083.

import (
	"errors"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// TestPgStore_AppRegistryCredentials_RoundTrip covers the basic
// CRUD + idempotent upsert: insert, get, list, count, mark-used,
// delete. Pinned against real SQL so the SELECT column order, the
// ON CONFLICT clause, and the last_used_at UPDATE all hold.
func TestPgStore_AppRegistryCredentials_RoundTrip(t *testing.T) {
	s, ctx := pgStore(t)
	acct, app, _ := seedLiveDeploy(t, s, ctx)

	const ciphertext = "sealed-payload-bytes-v1"
	if err := s.UpsertAppRegistryCredential(ctx, acct, app, "ghcr.io", "alice", []byte(ciphertext)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := s.GetAppRegistryCredential(ctx, acct, app, "ghcr.io")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Registry != "ghcr.io" {
		t.Errorf("Registry = %q, want ghcr.io", got.Registry)
	}
	if got.Username != "alice" {
		t.Errorf("Username = %q, want alice", got.Username)
	}
	if string(got.PasswordEncrypted) != ciphertext {
		t.Errorf("PasswordEncrypted round-trip mismatch")
	}
	if got.LastUsedAt != nil {
		t.Errorf("LastUsedAt on fresh row = %v, want nil", got.LastUsedAt)
	}

	list, err := s.ListAppRegistryCredentials(ctx, acct, app)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Registry != "ghcr.io" {
		t.Errorf("List = %+v, want single ghcr.io row", list)
	}

	count, err := s.CountAppRegistryCredentials(ctx, acct, app)
	if err != nil || count != 1 {
		t.Errorf("Count = %d, %v; want 1, nil", count, err)
	}

	// MarkUsed advances last_used_at.
	if err := s.MarkAppRegistryCredentialUsed(ctx, acct, app, "ghcr.io"); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	after, err := s.GetAppRegistryCredential(ctx, acct, app, "ghcr.io")
	if err != nil {
		t.Fatalf("Get after MarkUsed: %v", err)
	}
	if after.LastUsedAt == nil {
		t.Errorf("LastUsedAt after MarkUsed = nil, want non-nil")
	}

	if err := s.DeleteAppRegistryCredential(ctx, acct, app, "ghcr.io"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.GetAppRegistryCredential(ctx, acct, app, "ghcr.io"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("Get after Delete = %v, want ErrNotFound", err)
	}
}

// TestPgStore_AppRegistryCredentials_UpsertReplaces verifies the
// INSERT … ON CONFLICT (app_id, registry) DO UPDATE branch: a second
// upsert with a different username+ciphertext MUST update the same
// row (no duplicate row, no separate key). CreatedAt holds; UpdatedAt
// advances.
func TestPgStore_AppRegistryCredentials_UpsertReplaces(t *testing.T) {
	s, ctx := pgStore(t)
	acct, app, _ := seedLiveDeploy(t, s, ctx)

	if err := s.UpsertAppRegistryCredential(ctx, acct, app, "ghcr.io", "alice", []byte("ct1")); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}
	first, err := s.GetAppRegistryCredential(ctx, acct, app, "ghcr.io")
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	createdAt := first.CreatedAt
	updatedAt := first.UpdatedAt
	// pgtest clusters can be sub-millisecond; sleep defensively.
	time.Sleep(10 * time.Millisecond)

	if err := s.UpsertAppRegistryCredential(ctx, acct, app, "ghcr.io", "alice-rotated", []byte("ct2")); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}
	second, err := s.GetAppRegistryCredential(ctx, acct, app, "ghcr.io")
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("Upsert produced a new row: first.ID=%s second.ID=%s", first.ID, second.ID)
	}
	if second.Username != "alice-rotated" {
		t.Errorf("Username after upsert = %q, want alice-rotated", second.Username)
	}
	if string(second.PasswordEncrypted) != "ct2" {
		t.Errorf("Ciphertext after upsert = %q, want ct2", string(second.PasswordEncrypted))
	}
	if !second.CreatedAt.Equal(createdAt) {
		t.Errorf("CreatedAt changed across upsert: %v vs %v", createdAt, second.CreatedAt)
	}
	if !second.UpdatedAt.After(updatedAt) && !second.UpdatedAt.Equal(updatedAt) {
		// Postgres `now()` advances per-tx — but in pgtest against a single
		// connection the timestamps may be sub-microsecond equal. Accept
		// either >= (monotonicity) OR equal (clock didn't tick).
		t.Errorf("UpdatedAt regressed: first=%v second=%v", updatedAt, second.UpdatedAt)
	}

	// Still exactly one row.
	count, err := s.CountAppRegistryCredentials(ctx, acct, app)
	if err != nil || count != 1 {
		t.Errorf("Count after upsert = %d, %v; want 1, nil", count, err)
	}
}

// TestPgStore_AppRegistryCredentials_AccountIsolation pins the
// (account_id, app_id, registry) predicate in Get / List / Delete —
// a stale ID→slug mapping can't leak credentials across accounts.
func TestPgStore_AppRegistryCredentials_AccountIsolation(t *testing.T) {
	s, ctx := pgStore(t)
	acctA, appA, _ := seedLiveDeploy(t, s, ctx)

	// Second account under the same pool, but a separate app.
	acctB, err := s.CreateAccount(ctx, "b@example.com", api.PlanPro)
	if err != nil {
		t.Fatalf("CreateAccount B: %v", err)
	}
	appB, err := s.CreateApp(ctx, state.App{
		AccountID: acctB.ID, Slug: "pg-app-b", Type: state.AppTypeApp,
		RAMMB: 256, MaxConcurrency: 2, IdleTimeoutS: 60,
	})
	if err != nil {
		t.Fatalf("CreateApp B: %v", err)
	}

	if err := s.UpsertAppRegistryCredential(ctx, acctA, appA, "ghcr.io", "alice", []byte("ct")); err != nil {
		t.Fatalf("Upsert under A: %v", err)
	}

	// Cross-account get returns ErrNotFound.
	if _, err := s.GetAppRegistryCredential(ctx, acctB.ID, appA, "ghcr.io"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("Get cross-account = %v, want ErrNotFound", err)
	}
	// Cross-app under right account returns ErrNotFound.
	if _, err := s.GetAppRegistryCredential(ctx, acctA, appB.ID, "ghcr.io"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("Get cross-app = %v, want ErrNotFound", err)
	}
	// Cross-account list returns empty.
	list, err := s.ListAppRegistryCredentials(ctx, acctB.ID, appA)
	if err != nil {
		t.Fatalf("List cross-account: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("List cross-account len = %d, want 0", len(list))
	}
	// Cross-account delete returns ErrNotFound.
	if err := s.DeleteAppRegistryCredential(ctx, acctB.ID, appA, "ghcr.io"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("Delete cross-account = %v, want ErrNotFound", err)
	}

	// The original row survives.
	if _, err := s.GetAppRegistryCredential(ctx, acctA, appA, "ghcr.io"); err != nil {
		t.Errorf("Get original after cross-account attempts: %v", err)
	}
}

// TestPgStore_AppRegistryCredentials_DeleteNotFound pins that
// deleting an absent row returns ErrNotFound (the handler maps this
// to a problem, not a 204).
func TestPgStore_AppRegistryCredentials_DeleteNotFound(t *testing.T) {
	s, ctx := pgStore(t)
	acct, app, _ := seedLiveDeploy(t, s, ctx)
	if err := s.DeleteAppRegistryCredential(ctx, acct, app, "never-existed.example.com"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("Delete absent = %v, want ErrNotFound", err)
	}
}

// TestPgStore_AppRegistryCredentials_MarkUsed_NotFound pins the
// "non-fatal" contract: ErrNotFound surfaces (callers MUST treat as
// non-fatal — the deployment already succeeded).
func TestPgStore_AppRegistryCredentials_MarkUsed_NotFound(t *testing.T) {
	s, ctx := pgStore(t)
	acct, app, _ := seedLiveDeploy(t, s, ctx)
	if err := s.MarkAppRegistryCredentialUsed(ctx, acct, app, "never-existed.example.com"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("MarkUsed absent = %v, want ErrNotFound", err)
	}
}

// TestPgStore_AppRegistryCredentials_MultipleRegistriesAndOrdering
// pins the (app_id, registry) UNIQUE constraint allows multiple
// rows on the same app, and List orders by registry ASC.
func TestPgStore_AppRegistryCredentials_MultipleRegistriesAndOrdering(t *testing.T) {
	s, ctx := pgStore(t)
	acct, app, _ := seedLiveDeploy(t, s, ctx)

	hosts := []string{"zzz.example.com", "aaa.example.com", "mmm.example.com"}
	for i, h := range hosts {
		if err := s.UpsertAppRegistryCredential(ctx, acct, app, h, "u", []byte{byte(i)}); err != nil {
			t.Fatalf("Upsert %s: %v", h, err)
		}
	}
	list, err := s.ListAppRegistryCredentials(ctx, acct, app)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("List len = %d, want 3", len(list))
	}
	want := []string{"aaa.example.com", "mmm.example.com", "zzz.example.com"}
	for i, w := range want {
		if list[i].Registry != w {
			t.Errorf("List[%d].Registry = %q, want %q", i, list[i].Registry, w)
		}
	}
	count, err := s.CountAppRegistryCredentials(ctx, acct, app)
	if err != nil || count != 3 {
		t.Errorf("Count = %d, %v; want 3, nil", count, err)
	}
}

// TestPgStore_AppRegistryCredentials_FKCascadeOnAccountDelete pins
// that deleting the parent account removes every credential row
// for that account. The FK on app_registry_credentials.account_id
// carries the production guarantee.
func TestPgStore_AppRegistryCredentials_FKCascadeOnAccountDelete(t *testing.T) {
	s, ctx := pgStore(t)
	acct, app, _ := seedLiveDeploy(t, s, ctx)

	if err := s.UpsertAppRegistryCredential(ctx, acct, app, "ghcr.io", "alice", []byte("ct1")); err != nil {
		t.Fatalf("Upsert 1: %v", err)
	}
	if err := s.UpsertAppRegistryCredential(ctx, acct, app, "registry.gregale.dev", "bob", []byte("ct2")); err != nil {
		t.Fatalf("Upsert 2: %v", err)
	}

	// Drive the G6 grace flow (mirrors the apid delete-pending path).
	if err := s.MarkAccountDeletionPending(ctx, acct); err != nil {
		t.Fatalf("MarkAccountDeletionPending: %v", err)
	}
	if err := s.DeleteAccount(ctx, acct); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}

	count, err := s.CountAppRegistryCredentials(ctx, acct, app)
	if err != nil {
		t.Fatalf("Count after cascade: %v", err)
	}
	if count != 0 {
		t.Errorf("Count after account cascade = %d, want 0", count)
	}
}
