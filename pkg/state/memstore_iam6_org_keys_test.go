// Memstore parity tests for the 5 IAM-6 org-bound API key methods
// (issue #190 / ADR-061, PR 6). Mirrors the pgstore parity tests at
// pgstore_iam5_keys_test.go — every Store method that the new
// handlers reach gets the same coverage on both backends so a
// divergence trips the build, not a customer.
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

func TestMemStore_OrgAPIKey_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := state.NewMemStore()
	acct, err := s.CreateAccount(ctx, "iam6-org-mem@example.com", api.PlanHobby)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	orgID := "org-" + uuid.NewString()

	// Create
	hash := []byte{0x01, 0x02, 0x03, 0x04}
	exp := time.Now().Add(24 * time.Hour)
	k, err := s.CreateOrgAPIKey(ctx, orgID, acct.ID, hash, "ci-deploy", []string{"deploy:write"}, &exp)
	if err != nil {
		t.Fatalf("CreateOrgAPIKey: %v", err)
	}
	if k.OrgID != orgID {
		t.Errorf("OrgID: got %q, want %q", k.OrgID, orgID)
	}
	if k.AccountID != acct.ID {
		t.Errorf("AccountID: got %q, want %q", k.AccountID, acct.ID)
	}
	if k.Status != string(state.APIKeyStatusActive) {
		t.Errorf("Status: got %q, want active", k.Status)
	}
	if k.ExpiresAt == nil || !k.ExpiresAt.Equal(exp) {
		t.Errorf("ExpiresAt: got %v, want %v", k.ExpiresAt, exp)
	}

	// Get (idempotent + IDOR-safe across org)
	got, err := s.GetOrgAPIKey(ctx, orgID, k.ID)
	if err != nil {
		t.Fatalf("GetOrgAPIKey: %v", err)
	}
	if got.ID != k.ID {
		t.Errorf("GetOrgAPIKey ID: got %q, want %q", got.ID, k.ID)
	}
	if _, err := s.GetOrgAPIKey(ctx, "wrong-org", k.ID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("GetOrgAPIKey cross-org: got err=%v, want ErrNotFound", err)
	}

	// List filters by org_id + skips revoked
	other, err := s.CreateOrgAPIKey(ctx, "org-other", acct.ID, []byte{0x05}, "other", []string{"admin"}, nil)
	if err != nil {
		t.Fatalf("CreateOrgAPIKey(other): %v", err)
	}
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
	_ = other

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
	// List should now skip the revoked key
	keys, err = s.ListOrgAPIKeys(ctx, orgID)
	if err != nil {
		t.Fatalf("ListOrgAPIKeys after revoke: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("ListOrgAPIKeys after revoke: got %d, want 0 (revoked rows excluded)", len(keys))
	}
	if _, err := s.RevokeOrgAPIKey(ctx, "wrong-org", k.ID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("RevokeOrgAPIKey cross-org: got err=%v, want ErrNotFound", err)
	}

	// Duplicate hash rejected
	if _, err := s.CreateOrgAPIKey(ctx, orgID, acct.ID, hash, "dup", []string{"admin"}, nil); err == nil {
		t.Errorf("CreateOrgAPIKey duplicate hash did not error")
	}

	// Unknown account → ErrNotFound
	if _, err := s.CreateOrgAPIKey(ctx, orgID, "no-such-acct", []byte{0x09}, "x", nil, nil); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("CreateOrgAPIKey unknown account: got err=%v, want ErrNotFound", err)
	}
}

func TestMemStore_OrgAPIKey_Rotate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := state.NewMemStore()
	acct, err := s.CreateAccount(ctx, "iam6-org-mem-rotate@example.com", api.PlanHobby)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	orgID := "org-" + uuid.NewString()

	old, err := s.CreateOrgAPIKey(ctx, orgID, acct.ID, []byte{0x10}, "to-rotate", []string{"admin"}, nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Happy path: rotate with 24h grace.
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

	// Cross-org 404
	if _, _, err := s.RotateOrgAPIKey(ctx, "wrong-org", old.ID, []byte{0x21}, "x", 0); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("RotateOrgAPIKey cross-org: got err=%v, want ErrNotFound", err)
	}

	// Rotating an already-revoked key → ErrAPIKeyRevoked.
	old2, err := s.CreateOrgAPIKey(ctx, orgID, acct.ID, []byte{0x11}, "to-revoke", []string{"admin"}, nil)
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

func TestMemStore_GetAPIKey_IDORCollapse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := state.NewMemStore()
	acct, err := s.CreateAccount(ctx, "iam6-getapikey@example.com", api.PlanHobby)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	other, err := s.CreateAccount(ctx, "iam6-getapikey-other@example.com", api.PlanHobby)
	if err != nil {
		t.Fatalf("create other: %v", err)
	}
	k, err := s.CreateAPIKeyWithExpiry(ctx, acct.ID, []byte{0x30}, "owner", []string{"admin"}, nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Cross-account read must collapse to ErrNotFound.
	if _, err := s.GetAPIKey(ctx, other.ID, k.ID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("cross-account GetAPIKey: got err=%v, want ErrNotFound", err)
	}
	// Owner can read it.
	if got, err := s.GetAPIKey(ctx, acct.ID, k.ID); err != nil || got.ID != k.ID {
		t.Errorf("owner GetAPIKey: got err=%v id=%q, want nil/%q", err, got.ID, k.ID)
	}
}
