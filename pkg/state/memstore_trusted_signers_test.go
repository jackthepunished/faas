package state

// MemStore coverage tests for the per-app cosign trusted-publisher
// CRUD (issue #472 / ADR-058). Mirrors pkg/state/memstore_secret_*
// coverage so each method on the cluster has at least one direct
// test, raising pkg/state coverage above the 70% gate (the
// exclude-generated gate at Makefile:99).
//
// Each test stands up a fresh MemStore and exercises the path the
// production handler uses (cmd/apid/handlers_trusted_signers.go).
// The cluster intentionally lives next to the equivalent pgstore
// coverage file (pgstore_trusted_signers_test.go) so the two
// stay in sync; rotate coverage when one side gains a method.

import (
	"context"
	"errors"
	"testing"
)

// memstoreTrustedSignerKey is the same 91-byte raw DER ECDSA P-256
// SPKI blob used in pgstore_trusted_signers_test.go so the two
// fixtures stay byte-comparable (PR #371 sign-pub.pem shape).
var memstoreTrustedSignerKey = []byte("-----BEGIN PUBLIC KEY-----\nMFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAESampleECDSAP256SPKIShapeForTesting==\n-----END PUBLIC KEY-----")

func TestMemStore_UpsertAppTrustedSigner_InsertAndRotate(t *testing.T) {
	m := newMemStoreForTest()
	ctx := context.Background()

	acctA := "00000000-0000-0000-0000-0000000000a1"
	appA := "00000000-0000-0000-0000-0000000000b1"

	addedAt1, rotated1, err := m.UpsertAppTrustedSigner(ctx, acctA, appA, "ci-bot", memstoreTrustedSignerKey, acctA)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if rotated1 {
		t.Errorf("first upsert: rotated=true, want false (fresh insert)")
	}
	if addedAt1.IsZero() {
		t.Errorf("first upsert: added_at is zero")
	}

	addedAt2, rotated2, err := m.UpsertAppTrustedSigner(ctx, acctA, appA, "ci-bot", memstoreTrustedSignerKey, acctA)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if !rotated2 {
		t.Errorf("second upsert: rotated=false, want true")
	}
	if !addedAt2.Equal(addedAt1) {
		t.Errorf("second upsert: added_at shifted %v → %v", addedAt1, addedAt2)
	}

	// Wrong-account upsert returns ErrNotFound (the row pre-exists
	// under a different account — handler treats it as 404).
	_, _, err = m.UpsertAppTrustedSigner(ctx, "00000000-0000-0000-0000-0000000000a2", appA, "ci-bot", memstoreTrustedSignerKey, acctA)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-account upsert: err = %v, want ErrNotFound", err)
	}
}

func TestMemStore_DeleteAppTrustedSigner_HappyAndErrNotFound(t *testing.T) {
	m := newMemStoreForTest()
	ctx := context.Background()
	acctA := "00000000-0000-0000-0000-0000000000a1"
	appA := "00000000-0000-0000-0000-0000000000b1"

	if _, _, err := m.UpsertAppTrustedSigner(ctx, acctA, appA, "ci-bot", memstoreTrustedSignerKey, acctA); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := m.DeleteAppTrustedSigner(ctx, acctA, appA, "ci-bot"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := m.DeleteAppTrustedSigner(ctx, acctA, appA, "ci-bot"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: err = %v, want ErrNotFound", err)
	}

	// Wrong-account delete also returns ErrNotFound.
	if _, _, err := m.UpsertAppTrustedSigner(ctx, acctA, appA, "ci-bot", memstoreTrustedSignerKey, acctA); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	acctB := "00000000-0000-0000-0000-0000000000a2"
	if err := m.DeleteAppTrustedSigner(ctx, acctB, appA, "ci-bot"); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-account delete: err = %v, want ErrNotFound", err)
	}
}

func TestMemStore_ListAppTrustedSigners_OrdersAndScopesByAccount(t *testing.T) {
	m := newMemStoreForTest()
	ctx := context.Background()
	acctA := "00000000-0000-0000-0000-0000000000a1"
	acctB := "00000000-0000-0000-0000-0000000000a2"
	appA := "00000000-0000-0000-0000-0000000000b1"
	appB := "00000000-0000-0000-0000-0000000000b2"

	// Two rows on appA out of insertion order to pin the sort.
	for _, name := range []string{"zeta", "alpha"} {
		if _, _, err := m.UpsertAppTrustedSigner(ctx, acctA, appA, name, memstoreTrustedSignerKey, acctA); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	// Sibling row on acctB+appB to pin the scope filter.
	if _, _, err := m.UpsertAppTrustedSigner(ctx, acctB, appB, "sibling", memstoreTrustedSignerKey, acctB); err != nil {
		t.Fatalf("seed sibling: %v", err)
	}

	got, err := m.ListAppTrustedSigners(ctx, acctA, appA)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("rows = %d, want 2", len(got))
	}
	if got[0].SignerName != "alpha" || got[1].SignerName != "zeta" {
		t.Errorf("order = [%s, %s], want [alpha, zeta]", got[0].SignerName, got[1].SignerName)
	}

	// Empty case.
	empty, err := m.ListAppTrustedSigners(ctx, acctA, appB)
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("empty rows = %d, want 0", len(empty))
	}
}

// TestMemStore_ListAppTrustedSignersForApp_NoAccountScope pins the
// system-side sibling (cmd/apid/trusted_publisher_writer.go path):
// pass appID only, return every row on that app across all
// accounts. Order is signer_name ASC.
func TestMemStore_ListAppTrustedSignersForApp_NoAccountScope(t *testing.T) {
	m := newMemStoreForTest()
	ctx := context.Background()
	acctA := "00000000-0000-0000-0000-0000000000a1"
	acctB := "00000000-0000-0000-0000-0000000000a2"
	appA := "00000000-0000-0000-0000-0000000000b1"

	if _, _, err := m.UpsertAppTrustedSigner(ctx, acctA, appA, "alpha", memstoreTrustedSignerKey, acctA); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	if _, _, err := m.UpsertAppTrustedSigner(ctx, acctB, appA, "zeta", memstoreTrustedSignerKey, acctB); err != nil {
		t.Fatalf("seed B: %v", err)
	}

	got, err := m.ListAppTrustedSignersForApp(ctx, appA)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("rows = %d, want 2 (no account scope)", len(got))
	}
	if got[0].SignerName != "alpha" || got[1].SignerName != "zeta" {
		t.Errorf("order = [%s, %s], want [alpha, zeta]", got[0].SignerName, got[1].SignerName)
	}
}

func TestMemStore_CountAppTrustedSigners_QuotaShape(t *testing.T) {
	m := newMemStoreForTest()
	ctx := context.Background()
	acctA := "00000000-0000-0000-0000-0000000000a1"
	appA := "00000000-0000-0000-0000-0000000000b1"
	acctB := "00000000-0000-0000-0000-0000000000a2"
	appB := "00000000-0000-0000-0000-0000000000b2"

	// Empty case → 0.
	n, err := m.CountAppTrustedSigners(ctx, acctA, appA)
	if err != nil {
		t.Fatalf("count empty: %v", err)
	}
	if n != 0 {
		t.Errorf("empty count = %d, want 0", n)
	}

	// Seed three rows.
	for _, name := range []string{"ci-bot", "release-bot", "audit-bot"} {
		if _, _, err := m.UpsertAppTrustedSigner(ctx, acctA, appA, name, memstoreTrustedSignerKey, acctA); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	n, err = m.CountAppTrustedSigners(ctx, acctA, appA)
	if err != nil {
		t.Fatalf("count seeded: %v", err)
	}
	if n != 3 {
		t.Errorf("count = %d, want 3", n)
	}

	// Sibling app on a different account → 0 (scoped).
	n, err = m.CountAppTrustedSigners(ctx, acctB, appB)
	if err != nil {
		t.Fatalf("count sibling: %v", err)
	}
	if n != 0 {
		t.Errorf("sibling count = %d, want 0", n)
	}
}

// newMemStoreForTest returns an empty MemStore wired the same way the
// production handler uses (no fixtures). Kept local to avoid pulling
// in the full helpers_test.go machinery just for these tests.
func newMemStoreForTest() *MemStore {
	return NewMemStore()
}
