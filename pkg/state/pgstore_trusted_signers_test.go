package state_test

// PgStore coverage tests for the per-app cosign trusted-publisher
// CRUD (issue #472 / ADR-058). Mirrors the secret-CRUD pgstore
// coverage slice so each method on the cluster has at least one
// direct test:
//
//   - UpsertAppTrustedSigner      — insert + rotated-on-conflict semantics
//   - DeleteAppTrustedSigner      — happy + ErrNotFound path
//   - ListAppTrustedSigners       — empty + ordered + scoped-by-account
//   - ListAppTrustedSignersForApp — system-side sibling (no accountID)
//   - CountAppTrustedSigners      — quota-count helper
//
// The upsert `(xmax = 0)` idiom returns rotated=false on first write
// and rotated=true on subsequent PUTs; the test pins both halves
// because a regression that drops the boolean column would silently
// misclassify every audit row. The "ECDSA P-256 SPKI is exactly 91
// bytes raw DER" fixture from migration 00083 is reused as the blob
// payload — a future change to the CHECK shape would surface here as
// a 22P02 surfacing from the insert path.
//
// Build tag matches the rest of the pgstore tests; set
// FAAS_SKIP_PG_TESTS=1 to skip locally (see migrations/README.md).

import (
	"errors"
	"testing"

	"github.com/onebox-faas/faas/pkg/state"
)

// trustedSignerKeyPEM is a 91-byte raw DER ECDSA P-256 SPKI blob,
// sized to satisfy the app_trusted_signers_pem_shape CHECK
// (octet_length BETWEEN 64 AND 1024) and reflect the shape PR #371
// writes under /etc/faas/secrets/sign-pub.pem. The literal bytes
// are arbitrary; the test only inspects round-trip equality.
var trustedSignerKeyPEM = []byte("-----BEGIN PUBLIC KEY-----\nMFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAESampleECDSAP256SPKIShapeForTesting==\n-----END PUBLIC KEY-----")

func TestPg_UpsertAppTrustedSigner_InsertAndRotate(t *testing.T) {
	s, ctx := pgStore(t)
	acctID := createAccount(t, s, ctx, pgTestEmail(t))
	appID := createApp(t, s, ctx, acctID, "trust-upsert")

	// First write — rotated=false; added_at is fresh.
	addedAt1, rotated1, err := s.UpsertAppTrustedSigner(ctx, acctID, appID, "ci-bot", trustedSignerKeyPEM, acctID)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if rotated1 {
		t.Errorf("first upsert: rotated=true, want false (xmax=0 means fresh insert)")
	}
	if addedAt1.IsZero() {
		t.Errorf("first upsert: added_at is zero")
	}

	// Second write — rotated=true; added_at must stay at the original.
	addedAt2, rotated2, err := s.UpsertAppTrustedSigner(ctx, acctID, appID, "ci-bot", trustedSignerKeyPEM, acctID)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if !rotated2 {
		t.Errorf("second upsert: rotated=false, want true (xmax!=0 means update)")
	}
	if !addedAt2.Equal(addedAt1) {
		t.Errorf("second upsert: added_at shifted %v → %v (regression: rotation must preserve original timestamp)", addedAt1, addedAt2)
	}

	// Round-trip via List to pin the bytes survive.
	rows, err := s.ListAppTrustedSigners(ctx, acctID, appID)
	if err != nil {
		t.Fatalf("ListAppTrustedSigners: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListAppTrustedSigners rows = %d, want 1", len(rows))
	}
	if string(rows[0].CosignPublicKey) != string(trustedSignerKeyPEM) {
		t.Errorf("CosignPublicKey round-trip mismatch")
	}
	if rows[0].SignerName != "ci-bot" {
		t.Errorf("SignerName = %q, want %q", rows[0].SignerName, "ci-bot")
	}
	if rows[0].AddedByAccountID != acctID {
		t.Errorf("AddedByAccountID = %q, want %q", rows[0].AddedByAccountID, acctID)
	}
}

func TestPg_DeleteAppTrustedSigner_HappyAndErrNotFound(t *testing.T) {
	s, ctx := pgStore(t)
	acctID := createAccount(t, s, ctx, pgTestEmail(t))
	appID := createApp(t, s, ctx, acctID, "trust-delete")

	if _, _, err := s.UpsertAppTrustedSigner(ctx, acctID, appID, "ci-bot", trustedSignerKeyPEM, acctID); err != nil {
		t.Fatalf("seed upsert: %v", err)
	}
	// Happy path.
	if err := s.DeleteAppTrustedSigner(ctx, acctID, appID, "ci-bot"); err != nil {
		t.Fatalf("DeleteAppTrustedSigner: %v", err)
	}
	// Missing row → ErrNotFound.
	if err := s.DeleteAppTrustedSigner(ctx, acctID, appID, "ci-bot"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("second delete: err = %v, want ErrNotFound", err)
	}
}

func TestPg_ListAppTrustedSigners_OrdersAndScopesByAccount(t *testing.T) {
	s, ctx := pgStore(t)
	acctA := createAccount(t, s, ctx, pgTestEmail(t))
	acctB := createAccount(t, s, ctx, pgTestEmail(t))
	appA := createApp(t, s, ctx, acctA, "trust-list-a")
	appB := createApp(t, s, ctx, acctB, "trust-list-b")

	// Two rows on appA out of insertion order to pin the ASC order.
	for _, name := range []string{"zeta", "alpha"} {
		if _, _, err := s.UpsertAppTrustedSigner(ctx, acctA, appA, name, trustedSignerKeyPEM, acctA); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	// One row on appB to pin the account-scope filter.
	if _, _, err := s.UpsertAppTrustedSigner(ctx, acctB, appB, "sibling", trustedSignerKeyPEM, acctB); err != nil {
		t.Fatalf("seed sibling: %v", err)
	}

	got, err := s.ListAppTrustedSigners(ctx, acctA, appA)
	if err != nil {
		t.Fatalf("ListAppTrustedSigners: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("rows = %d, want 2 (account scope filter must exclude appB)", len(got))
	}
	if got[0].SignerName != "alpha" || got[1].SignerName != "zeta" {
		t.Errorf("order = [%s, %s], want [alpha, zeta] (signer_name ASC)", got[0].SignerName, got[1].SignerName)
	}

	// Empty case: an account with no signers.
	empty, err := s.ListAppTrustedSigners(ctx, acctA, appB)
	if err != nil {
		t.Fatalf("ListAppTrustedSigners empty: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("empty rows = %d, want 0", len(empty))
	}
}

// TestPg_ListAppTrustedSignersForApp_NoAccountScope pins the system-
// side sibling that the on-disk PEM mirror writer uses (cmd/apid/
// trusted_publisher_writer.go). The contract is: pass appID only,
// return every row on that app across all accounts (the writer runs
// as a system goroutine and has no accountID in scope).
func TestPg_ListAppTrustedSignersForApp_NoAccountScope(t *testing.T) {
	s, ctx := pgStore(t)
	acctA := createAccount(t, s, ctx, pgTestEmail(t))
	appA := createApp(t, s, ctx, acctA, "trust-list-system")

	if _, _, err := s.UpsertAppTrustedSigner(ctx, acctA, appA, "ci-bot", trustedSignerKeyPEM, acctA); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := s.ListAppTrustedSignersForApp(ctx, appA)
	if err != nil {
		t.Fatalf("ListAppTrustedSignersForApp: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("rows = %d, want 1", len(got))
	}
	if got[0].AppID != appA {
		t.Errorf("AppID = %q, want %q", got[0].AppID, appA)
	}
}

func TestPg_CountAppTrustedSigners_QuotaShape(t *testing.T) {
	s, ctx := pgStore(t)
	acctID := createAccount(t, s, ctx, pgTestEmail(t))
	appID := createApp(t, s, ctx, acctID, "trust-count")

	// Empty app → 0.
	n, err := s.CountAppTrustedSigners(ctx, acctID, appID)
	if err != nil {
		t.Fatalf("Count empty: %v", err)
	}
	if n != 0 {
		t.Errorf("empty count = %d, want 0", n)
	}

	// Seed three rows → 3.
	for _, name := range []string{"ci-bot", "release-bot", "audit-bot"} {
		if _, _, err := s.UpsertAppTrustedSigner(ctx, acctID, appID, name, trustedSignerKeyPEM, acctID); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	n, err = s.CountAppTrustedSigners(ctx, acctID, appID)
	if err != nil {
		t.Fatalf("Count seeded: %v", err)
	}
	if n != 3 {
		t.Errorf("count = %d, want 3", n)
	}

	// Sibling app on a different account → 0 (scoped by account_id
	// + app_id; mirrors CountAppSecrets).
	acctB := createAccount(t, s, ctx, pgTestEmail(t))
	appB := createApp(t, s, ctx, acctB, "trust-count-sibling")
	n, err = s.CountAppTrustedSigners(ctx, acctB, appB)
	if err != nil {
		t.Fatalf("Count sibling: %v", err)
	}
	if n != 0 {
		t.Errorf("sibling count = %d, want 0 (account scope)", n)
	}
}
