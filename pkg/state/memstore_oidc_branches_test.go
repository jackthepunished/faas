// memstore_oidc_branches_test.go — coverage for the
// in-process OIDC trust-policy + exchanged-token seams in
// pkg/state/memstore.go (issue #270 / ADR-101).
//
// The DB-bound surface for OIDC (upsert_policy.sql,
// find_exchanged_token.sql) needs Postgres. The MemStore
// paths are testable in isolation and pin:
//
//   - UpsertOIDCTrustPolicy happy path (insert)
//   - UpsertOIDCTrustPolicy update preserves CreatedAt
//   - GetOIDCTrustPolicy returns ErrNotFound on miss
//   - ListOIDCTrustPoliciesForAccount filters by account_id
//   - InsertOIDCExchangedToken happy path
//   - GetOIDCExchangedTokenByHash on hit + miss (ErrNotFound)
//   - DeleteOIDCExchangedToken happy + unknown-id idempotent
//   - regexpMatch: the inlined Go-regexp wrapper. Pins the
//     patterns OIDC-issuers actually use (claim-prefix,
//     email-domain, character classes).
//
// Whitebox test (package state) matching the memstore_*_test.go
// convention.
package state

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// TestRegexpMatch_HitsAndMisses pins the canonical Go regexp
// semantics over the OIDC-relevant pattern surface (issuer
// allowlist subjects, claim-prefix, etc.).
func TestRegexpMatch_HitsAndMisses(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{`^ops@`, "ops@gregale.dev", true},
		{`^ops@`, "user@gregale.dev", false},
		{`@gregale\.dev$`, "alice@gregale.dev", true},
		{`@gregale\.dev$`, "alice@evil.dev", false},
		// Character class
		{`^[a-z]+$`, "abc", true},
		{`^[a-z]+$`, "ABC", false},
		{`^[a-z]+$`, "abc123", false},
		// Wildcard
		{`.*`, "", true},
		{`^.*$`, "anything goes here", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run("", func(t *testing.T) {
			if got := regexpMatch(tc.pattern, tc.s); got != tc.want {
				t.Errorf("regexpMatch(%q, %q) = %v, want %v", tc.pattern, tc.s, got, tc.want)
			}
		})
	}
}

// TestUpsertOIDCTrustPolicy_InsertNew pins the first-time path:
// no existing row, CreatedAt is server-stamped to a non-zero
// value, the row is reachable via GetOIDCTrustPolicy.
func TestUpsertOIDCTrustPolicy_InsertNew(t *testing.T) {
	m := NewMemStore()
	p := &OIDCTrustPolicy{
		AccountID:      "acct-1",
		IssuerURL:      "https://accounts.google.com",
		JWKSURL:        "https://accounts.google.com/.well-known/jwks.json",
		SubjectPattern: "^ops@",
	}
	got, err := m.UpsertOIDCTrustPolicy(context.Background(), p)
	if err != nil {
		t.Fatalf("UpsertOIDCTrustPolicy: %v", err)
	}
	if got == nil {
		t.Fatal("returned policy is nil")
	}
	if got.CreatedAt.IsZero() {
		t.Error("new policy CreatedAt is zero (server stamp missing)")
	}
	// Read-back via Lookup.
	fetch, err := m.GetOIDCTrustPolicy(context.Background(), "acct-1", "https://accounts.google.com")
	if err != nil {
		t.Fatalf("GetOIDCTrustPolicy: %v", err)
	}
	if fetch.IssuerURL != "https://accounts.google.com" {
		t.Errorf("Get back IssuerURL = %q, want original", fetch.IssuerURL)
	}
}

// TestUpsertOIDCTrustPolicy_UpdatePreservesCreatedAt pins the
// "preserve CreatedAt across upserts" contract: a second
// upsert for the same (account_id, issuer_url) does NOT
// overwrite the original CreatedAt. The audit-reader depends
// on stable CreatedAt to identify "first-use" timestamps.
func TestUpsertOIDCTrustPolicy_UpdatePreservesCreatedAt(t *testing.T) {
	m := NewMemStore()
	first := &OIDCTrustPolicy{
		AccountID: "acct-2",
		IssuerURL: "https://idp.example.com",
	}
	if _, err := m.UpsertOIDCTrustPolicy(context.Background(), first); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	originalCreatedAt := first.CreatedAt

	// Sleep a measurable delta so we can detect an UpdateAt drift
	// independent of CreatedAt preservation.
	time.Sleep(2 * time.Millisecond)

	second := &OIDCTrustPolicy{
		AccountID: "acct-2",
		IssuerURL: "https://idp.example.com",
		// Same key — exercises the update branch.
	}
	if _, err := m.UpsertOIDCTrustPolicy(context.Background(), second); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if !second.CreatedAt.Equal(originalCreatedAt) {
		t.Errorf("CreatedAt drifted on update: original=%v second=%v", originalCreatedAt, second.CreatedAt)
	}
	if second.UpdatedAt.Before(originalCreatedAt) {
		t.Errorf("UpdatedAt = %v not after CreatedAt = %v", second.UpdatedAt, originalCreatedAt)
	}
}

// TestGetOIDCTrustPolicy_NotFound pins the miss path: a
// GetOIDCTrustPolicy for an unknown key returns ErrNotFound.
func TestGetOIDCTrustPolicy_NotFound(t *testing.T) {
	m := NewMemStore()
	_, err := m.GetOIDCTrustPolicy(context.Background(), "nope", "https://unknown.example.com")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(unknown): err = %v, want ErrNotFound", err)
	}
}

// TestListOIDCTrustPoliciesForAccount_FiltersByAccount pins the
// account-scoped list: policies for other accounts must not
// appear in the returned slice.
func TestListOIDCTrustPoliciesForAccount_FiltersByAccount(t *testing.T) {
	m := NewMemStore()
	// Insert for two accounts.
	for _, p := range []*OIDCTrustPolicy{
		{AccountID: "acct-a", IssuerURL: "https://idp1.example.com"},
		{AccountID: "acct-a", IssuerURL: "https://idp2.example.com"},
		{AccountID: "acct-b", IssuerURL: "https://idp3.example.com"},
	} {
		if _, err := m.UpsertOIDCTrustPolicy(context.Background(), p); err != nil {
			t.Fatalf("seed %v: %v", p, err)
		}
	}
	a, err := m.ListOIDCTrustPoliciesForAccount(context.Background(), "acct-a")
	if err != nil {
		t.Fatalf("List(acct-a): %v", err)
	}
	if len(a) != 2 {
		t.Errorf("List(acct-a) returned %d policies, want 2", len(a))
	}
	for _, p := range a {
		if p.AccountID != "acct-a" {
			t.Errorf("cross-account leak: %q appears in acct-a list", p.AccountID)
		}
	}
}

// TestInsertOIDCExchangedToken_HappyPath pins the row-creation
// branch. The TokenHash gets hex-encoded into the map key, the
// row is reachable via GetOIDCExchangedTokenByHash.
func TestInsertOIDCExchangedToken_HappyPath(t *testing.T) {
	m := NewMemStore()
	hash := bytes.Repeat([]byte{0xAB}, 32)
	tok := &OIDCExchangedToken{
		ID:        "exch-1",
		AccountID: "acct-1",
		TokenHash: hash,
		IssuerURL: "https://accounts.google.com",
	}
	if _, err := m.InsertOIDCExchangedToken(context.Background(), tok); err != nil {
		t.Fatalf("InsertOIDCExchangedToken: %v", err)
	}
	fetch, err := m.GetOIDCExchangedTokenByHash(context.Background(), hash)
	if err != nil {
		t.Fatalf("GetOIDCExchangedTokenByHash: %v", err)
	}
	if fetch.ID != "exch-1" {
		t.Errorf("Get back ID = %q, want exch-1", fetch.ID)
	}
}

// TestGetOIDCExchangedTokenByHash_NotFound pins the miss path:
// an unknown hash returns ErrNotFound.
func TestGetOIDCExchangedTokenByHash_NotFound(t *testing.T) {
	m := NewMemStore()
	hash := bytes.Repeat([]byte{0x00}, 32)
	_, err := m.GetOIDCExchangedTokenByHash(context.Background(), hash)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(unknown hash): err = %v, want ErrNotFound", err)
	}
}

// TestDeleteOIDCExchangedToken_HappyAndErrorOnUnknown pins two
// paths: delete-an-existing-row succeeds (and the row is gone
// afterwards), and delete-of-unknown-id surfaces ErrNotFound
// (the documented contract — operator-driven revoke MUST be
// able to distinguish "row never existed" from "row gone
// silently" so an idempotency guard doesn't apply here).
func TestDeleteOIDCExchangedToken_HappyAndErrorOnUnknown(t *testing.T) {
	m := NewMemStore()
	hash := bytes.Repeat([]byte{0xCD}, 32)
	tok := &OIDCExchangedToken{
		ID:        "exch-x",
		AccountID: "acct-x",
		TokenHash: hash,
	}
	if _, err := m.InsertOIDCExchangedToken(context.Background(), tok); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Happy path.
	if err := m.DeleteOIDCExchangedToken(context.Background(), "exch-x"); err != nil {
		t.Errorf("Delete(existing): err = %v, want nil", err)
	}
	if _, err := m.GetOIDCExchangedTokenByHash(context.Background(), hash); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete: err = %v, want ErrNotFound", err)
	}

	// Re-delete of the just-deleted id: ErrNotFound (the row is
	// gone).
	if err := m.DeleteOIDCExchangedToken(context.Background(), "exch-x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete of just-deleted: err = %v, want ErrNotFound", err)
	}
	// Unknown id from the start also returns ErrNotFound.
	if err := m.DeleteOIDCExchangedToken(context.Background(), "never-existed"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete(never-existed): err = %v, want ErrNotFound", err)
	}
}
