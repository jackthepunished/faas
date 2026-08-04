package state_test

// PgStore parity tests for the IAM-6 (issue #190) org-bound key
// surface. Mirrors memstore_iam6_org_keys_test.go — every Store
// method that the new /v1/orgs/{slug}/keys/* handlers reach gets
// the same coverage on both backends so a divergence trips the
// build, not a customer.
//
// Methods under test:
//   CreateOrgAPIKey       — issue a new key stamped with org_id.
//   ListOrgAPIKeys        — filter by org_id, exclude revoked.
//   GetOrgAPIKey          — IDOR-safe (cross-org → ErrNotFound).
//   RevokeOrgAPIKey       — soft-delete + cross-org 404 collapse.
//   RotateOrgAPIKey       — atomic new + old-key demote + grace.
//   GetAPIKey             — legacy account-scoped lookup used by
//                           the dual-write delete/rotate paths.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/onebox-faas/faas/pkg/state"
)

func TestPg_OrgAPIKey_RoundTrip(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	acctID := createAccount(t, s, ctx, pgTestEmail(t))
	orgID := mustPersonalOrgID(t, ctx, pool, acctID)

	hash := []byte{0x01, 0x02, 0x03, 0x04}
	exp := time.Now().Add(24 * time.Hour)
	k, err := s.CreateOrgAPIKey(ctx, orgID, acctID, hash, "ci-deploy", []string{"deploy:write"}, &exp)
	if err != nil {
		t.Fatalf("CreateOrgAPIKey: %v", err)
	}
	if k.OrgID != orgID {
		t.Errorf("OrgID: got %q, want %q", k.OrgID, orgID)
	}
	if k.AccountID != acctID {
		t.Errorf("AccountID: got %q, want %q", k.AccountID, acctID)
	}
	if k.Status != string(state.APIKeyStatusActive) {
		t.Errorf("Status: got %q, want active", k.Status)
	}
	if k.ExpiresAt == nil {
		t.Errorf("ExpiresAt: nil, want set")
	}

	// Get (idempotent + IDOR-safe across org)
	got, err := s.GetOrgAPIKey(ctx, orgID, k.ID)
	if err != nil {
		t.Fatalf("GetOrgAPIKey: %v", err)
	}
	if got.ID != k.ID {
		t.Errorf("GetOrgAPIKey ID: got %q, want %q", got.ID, k.ID)
	}
	otherAcct := createAccount(t, s, ctx, pgTestEmail(t)+"-roundtrip-other")
	otherOrg := mustPersonalOrgID(t, ctx, pool, otherAcct)
	if _, err := s.GetOrgAPIKey(ctx, otherOrg, k.ID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("GetOrgAPIKey cross-org: got err=%v, want ErrNotFound", err)
	}

	// List filters by org_id + skips revoked
	keys, err := s.ListOrgAPIKeys(ctx, orgID)
	if err != nil {
		t.Fatalf("ListOrgAPIKeys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("ListOrgAPIKeys len = %d, want 1", len(keys))
	}
	if keys[0].ID != k.ID {
		t.Errorf("ListOrgAPIKeys returned key from another org")
	}

	// Revoke (idempotent + cross-org 404 collapse)
	rev, err := s.RevokeOrgAPIKey(ctx, orgID, k.ID)
	if err != nil {
		t.Fatalf("RevokeOrgAPIKey: %v", err)
	}
	if rev.Status != string(state.APIKeyStatusRevoked) {
		t.Errorf("post-revoke Status: got %q, want revoked", rev.Status)
	}
	if rev.RevokedAt == nil {
		t.Errorf("post-revoke RevokedAt: nil, want set")
	}
	keys, err = s.ListOrgAPIKeys(ctx, orgID)
	if err != nil {
		t.Fatalf("ListOrgAPIKeys after revoke: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("ListOrgAPIKeys after revoke: got %d, want 0 (revoked rows excluded)", len(keys))
	}
	if _, err := s.RevokeOrgAPIKey(ctx, otherOrg, k.ID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("RevokeOrgAPIKey cross-org: got err=%v, want ErrNotFound", err)
	}
}

func TestPg_OrgAPIKey_Rotate(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	acctID := createAccount(t, s, ctx, pgTestEmail(t))
	orgID := mustPersonalOrgID(t, ctx, pool, acctID)

	old, err := s.CreateOrgAPIKey(ctx, orgID, acctID, []byte{0x10}, "to-rotate", []string{"admin"}, nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	newK, oldK, err := s.RotateOrgAPIKey(ctx, orgID, old.ID, []byte{0x20}, "rotated", 24*time.Hour)
	if err != nil {
		t.Fatalf("RotateOrgAPIKey: %v", err)
	}
	if oldK.Status != string(state.APIKeyStatusGrace) {
		t.Errorf("old key Status: got %q, want grace", oldK.Status)
	}
	if newK.RotatedFromID == nil || *newK.RotatedFromID != old.ID {
		t.Errorf("new key RotatedFromID: got %v, want %q", newK.RotatedFromID, old.ID)
	}
	if oldK.ExpiresAt == nil {
		t.Errorf("old key ExpiresAt: nil, want set during grace")
	}

	otherAcct := createAccount(t, s, ctx, pgTestEmail(t)+"-rotate-other")
	otherOrg := mustPersonalOrgID(t, ctx, pool, otherAcct)
	if _, _, err := s.RotateOrgAPIKey(ctx, otherOrg, old.ID, []byte{0x21}, "x", 0); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("RotateOrgAPIKey cross-org: got err=%v, want ErrNotFound", err)
	}

	// Revoked → ErrAPIKeyRevoked.
	old2, err := s.CreateOrgAPIKey(ctx, orgID, acctID, []byte{0x11}, "to-revoke", []string{"admin"}, nil)
	if err != nil {
		t.Fatalf("seed revoked-test: %v", err)
	}
	if _, err := s.RevokeOrgAPIKey(ctx, orgID, old2.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, _, err := s.RotateOrgAPIKey(ctx, orgID, old2.ID, []byte{0x22}, "x", 0); !errors.Is(err, state.ErrAPIKeyRevoked) {
		t.Errorf("RotateOrgAPIKey on revoked: got err=%v, want ErrAPIKeyRevoked", err)
	}
}

func TestPg_GetAPIKey_IDORCollapse(t *testing.T) {
	s, ctx := pgStore(t)
	acctID := createAccount(t, s, ctx, pgTestEmail(t))
	otherID := createAccount(t, s, ctx, pgTestEmail(t)+"-other")
	k, err := s.CreateAPIKeyWithExpiry(ctx, acctID, []byte{0x30}, "owner", []string{"admin"}, nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.GetAPIKey(ctx, otherID, k.ID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("cross-account GetAPIKey: got err=%v, want ErrNotFound", err)
	}
	if got, err := s.GetAPIKey(ctx, acctID, k.ID); err != nil || got.ID != k.ID {
		t.Errorf("owner GetAPIKey: got err=%v id=%q, want nil/%q", err, got.ID, k.ID)
	}
}

// mustPersonalOrgID resolves the freshly seeded account's personal
// org id (the only org a fresh test account has). Mirrors the
// account → org_id wiring the Store layer expects.
func mustPersonalOrgID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, acctID string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`select id from orgs where personal_owner_account_id = $1 and personal_org = true limit 1`,
		acctID).Scan(&id); err != nil {
		t.Fatalf("lookup personal org for %s: %v", acctID, err)
	}
	return id
}
