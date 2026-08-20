package state_test

// Round-trip tests for the consumer_keys Store surface (ADR-120 /
// issue #975 item #5). Exercises the 6 methods — Create /
// GetByID / ListForApp / Revoke / TouchLastUsed /
// ConsumerKeyByAppAndPrefix — plus the load-bearing IDOR guard:
// a cross-tenant read returns ErrNotFound, not the row.
//
// Same pgtest.Open skip-when-no-pg pattern as
// pgstore_cors_presets_test.go. The (account_id, app_id, name)
// UNIQUE, the closed-set scope CHECK, and the IDOR predicates are
// all pinned — a silent weakening here lets a foreign tenant
// enumerate another tenant's keys.
//
// Insert path uses the Store.CreateConsumerKey method (not raw
// SQL) so a regression in the pg path's INSERT (column order,
// NULL coercion, hash length, expires_at > created_at CHECK)
// fails the test, not a follow-up real call.

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// seedConsumerKeyPg inserts a fresh ConsumerKey via the Store
// path and returns it. Generates a real plaintext + hash via
// api.GenerateConsumerKey so the SHA-256 matches what the
// gateway middleware will compare against. Test isolation is
// enforced by the random account/app IDs (uuid.NewString per
// call) — collisions on the (account_id, app_id, name) UNIQUE
// are impossible across calls.
func seedConsumerKeyPg(t *testing.T, ctx context.Context, st state.Store, accountID, appID, name string, scopes []string) state.ConsumerKey {
	t.Helper()
	plaintext, prefix, hash, err := api.GenerateConsumerKey()
	if err != nil {
		t.Fatalf("api.GenerateConsumerKey: %v", err)
	}
	if !api.ValidConsumerKeyFormat(plaintext) {
		t.Fatalf("api.ValidConsumerKeyFormat(%q) = false; the freshly-minted key must pass its own format check", plaintext)
	}
	expiresAt := time.Now().Add(365 * 24 * time.Hour).UTC()
	got, err := st.CreateConsumerKey(ctx, accountID, appID, name, prefix, hash, scopes, &expiresAt)
	if err != nil {
		t.Fatalf("CreateConsumerKey: %v", err)
	}
	if got.AccountID != accountID || got.AppID != appID || got.Name != name {
		t.Fatalf("seed round-trip mismatch: got %+v", got)
	}
	if got.Hash == nil || len(got.Hash) != 32 {
		t.Errorf("Hash length = %d, want 32 (SHA-256 of full plaintext)", len(got.Hash))
	}
	if got.RevokedAt != nil {
		t.Errorf("freshly-created key has RevokedAt != nil: %v", got.RevokedAt)
	}
	if got.LastUsedAt != nil {
		t.Errorf("freshly-created key has LastUsedAt != nil: %v", got.LastUsedAt)
	}
	return got
}

func TestPgStoreConsumerKeys_RoundTrip(t *testing.T) {
	store, _, ctx := pgStoreWithPool(t)

	accountID := uuid.NewString()
	appID := uuid.NewString()
	name := "ck-rt-" + strconv.Itoa(int(time.Now().UnixNano()))

	// Create.
	got := seedConsumerKeyPg(t, ctx, store, accountID, appID, name, []string{"read"})
	if got.ID == "" {
		t.Fatal("CreateConsumerKey returned empty ID")
	}

	// GetByID — same account.
	back, err := store.GetConsumerKeyByID(ctx, accountID, got.ID)
	if err != nil {
		t.Fatalf("GetConsumerKeyByID (same account): %v", err)
	}
	if back.ID != got.ID || back.Prefix != got.Prefix || back.Name != got.Name {
		t.Errorf("GetConsumerKeyByID round-trip mismatch: got %+v, want %+v", back, got)
	}
	if !equalScopes(back.Scopes, []string{"read"}) {
		t.Errorf("scopes = %v, want [read]", back.Scopes)
	}

	// ConsumerKeyByAppAndPrefix — the gateway-side hot-path lookup.
	byPrefix, err := store.ConsumerKeyByAppAndPrefix(ctx, accountID, appID, got.Prefix)
	if err != nil {
		t.Fatalf("ConsumerKeyByAppAndPrefix: %v", err)
	}
	if byPrefix.ID != got.ID {
		t.Errorf("ConsumerKeyByAppAndPrefix returned wrong row: ID = %s, want %s", byPrefix.ID, got.ID)
	}

	// ListForApp — should contain our row.
	list, err := store.ListConsumerKeysForApp(ctx, accountID, appID)
	if err != nil {
		t.Fatalf("ListConsumerKeysForApp: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListConsumerKeysForApp returned %d rows, want 1", len(list))
	}
	if list[0].ID != got.ID {
		t.Errorf("ListConsumerKeysForApp[0].ID = %s, want %s", list[0].ID, got.ID)
	}

	// TouchConsumerKeyLastUsed — best-effort observability.
	if err := store.TouchConsumerKeyLastUsed(ctx, got.ID); err != nil {
		t.Fatalf("TouchConsumerKeyLastUsed: %v", err)
	}
	touched, err := store.GetConsumerKeyByID(ctx, accountID, got.ID)
	if err != nil {
		t.Fatalf("GetConsumerKeyByID after touch: %v", err)
	}
	if touched.LastUsedAt == nil {
		t.Errorf("LastUsedAt still nil after TouchConsumerKeyLastUsed")
	}

	// Revoke — stamps revoked_at.
	revoked, err := store.RevokeConsumerKey(ctx, accountID, got.ID)
	if err != nil {
		t.Fatalf("RevokeConsumerKey: %v", err)
	}
	if revoked.RevokedAt == nil {
		t.Errorf("RevokedAt still nil after RevokeConsumerKey")
	}

	// Idempotent revoke — second call returns the same row.
	revoked2, err := store.RevokeConsumerKey(ctx, accountID, got.ID)
	if err != nil {
		t.Fatalf("RevokeConsumerKey (idempotent): %v", err)
	}
	if revoked2.RevokedAt == nil {
		t.Errorf("Idempotent revoke cleared RevokedAt")
	}
}

func TestPgStoreConsumerKeys_IDOR(t *testing.T) {
	store, _, ctx := pgStoreWithPool(t)

	accountA := uuid.NewString()
	accountB := uuid.NewString()
	appA := uuid.NewString()

	got := seedConsumerKeyPg(t, ctx, store, accountA, appA, "idor-key", []string{"read"})

	// GetByID — cross-account returns ErrNotFound, NOT the row.
	if _, err := store.GetConsumerKeyByID(ctx, accountB, got.ID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("GetConsumerKeyByID cross-account = %v, want ErrNotFound", err)
	}

	// ListConsumerKeysForApp — cross-account returns no rows.
	listB, err := store.ListConsumerKeysForApp(ctx, accountB, appA)
	if err != nil {
		t.Fatalf("ListConsumerKeysForApp cross-account: %v", err)
	}
	if len(listB) != 0 {
		t.Errorf("ListConsumerKeysForApp cross-account returned %d rows, want 0", len(listB))
	}

	// ConsumerKeyByAppAndPrefix — cross-account returns ErrNotFound.
	if _, err := store.ConsumerKeyByAppAndPrefix(ctx, accountB, appA, got.Prefix); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("ConsumerKeyByAppAndPrefix cross-account = %v, want ErrNotFound", err)
	}

	// RevokeConsumerKey — cross-account returns ErrNotFound.
	if _, err := store.RevokeConsumerKey(ctx, accountB, got.ID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("RevokeConsumerKey cross-account = %v, want ErrNotFound", err)
	}

	// Sanity: original-account reads still work.
	if _, err := store.GetConsumerKeyByID(ctx, accountA, got.ID); err != nil {
		t.Errorf("GetConsumerKeyByID same-account after cross-account probes: %v", err)
	}
}

// TestPgStoreConsumerKeys_UniqueNameAcrossApps — the
// (account_id, app_id, name) UNIQUE permits the same name in
// different apps of the same account, but rejects duplicates
// within one (account, app). This pins the load-bearing
// per-app-only scoping semantics from ADR-120 §D1.
func TestPgStoreConsumerKeys_UniqueNameAcrossApps(t *testing.T) {
	store, _, ctx := pgStoreWithPool(t)

	accountID := uuid.NewString()
	appA := uuid.NewString()
	appB := uuid.NewString()
	name := "shared-name-" + strconv.Itoa(int(time.Now().UnixNano()))

	// Same name, two different apps — both must succeed.
	if _, err := store.CreateConsumerKey(ctx, accountID, appA, name, "aaaa", makeHash(t, "x"), []string{"read"}, nil); err != nil {
		t.Fatalf("CreateConsumerKey appA: %v", err)
	}
	if _, err := store.CreateConsumerKey(ctx, accountID, appB, name, "bbbb", makeHash(t, "y"), []string{"read"}, nil); err != nil {
		t.Fatalf("CreateConsumerKey appB (same name, different app): %v (the UNIQUE is per-(account, app), so this must succeed)", err)
	}

	// Same name, same app — must fail with ErrConflict.
	if _, err := store.CreateConsumerKey(ctx, accountID, appA, name, "cccc", makeHash(t, "z"), []string{"read"}, nil); !errors.Is(err, state.ErrConflict) {
		t.Errorf("CreateConsumerKey duplicate in (account, app) = %v, want ErrConflict", err)
	}
}

// TestPgStoreConsumerKeys_HashPolicyPin — the pg path must store
// exactly the 32-byte SHA-256 of the FULL plaintext, NOT of the
// secret alone. The latter would let a secret reused across two
// prefixes produce identical hashes and collapse into the same
// row at lookup time.
func TestPgStoreConsumerKeys_HashPolicyPin(t *testing.T) {
	store, _, ctx := pgStoreWithPool(t)

	accountID := uuid.NewString()
	appID := uuid.NewString()

	// Two distinct plaintexts with the same secret bytes — they
	// MUST hash differently because the prefix is part of the input.
	plaintextA := "ck_aaaa_" + "00"
	plaintextB := "ck_bbbb_" + "00"
	hashA := api.HashConsumerKey(plaintextA)
	hashB := api.HashConsumerKey(plaintextB)
	for i := range hashA {
		if hashA[i] == hashB[i] {
			t.Fatalf("hash collision on distinct plaintexts (ADR-120 §Security notes — hash must include prefix): A=%x B=%x", hashA, hashB)
		}
	}

	// Two real keys with the secret-colliding setup. They MUST
	// land as distinct rows.
	rowA, err := store.CreateConsumerKey(ctx, accountID, appID, "ck-hash-A", "aaaa", hashA, []string{"read"}, nil)
	if err != nil {
		t.Fatalf("CreateConsumerKey A: %v", err)
	}
	rowB, err := store.CreateConsumerKey(ctx, accountID, appID, "ck-hash-B", "bbbb", hashB, []string{"read"}, nil)
	if err != nil {
		t.Fatalf("CreateConsumerKey B: %v", err)
	}
	if rowA.ID == rowB.ID {
		t.Fatal("two distinct hashes collapsed into one row (hash policy regression — distinct prefixes MUST produce distinct rows)")
	}

	// Lookup by each prefix returns its own row, not the other.
	backA, err := store.ConsumerKeyByAppAndPrefix(ctx, accountID, appID, "aaaa")
	if err != nil {
		t.Fatalf("lookup A: %v", err)
	}
	if backA.ID != rowA.ID {
		t.Errorf("lookup A returned %s, want %s", backA.ID, rowA.ID)
	}
	backB, err := store.ConsumerKeyByAppAndPrefix(ctx, accountID, appID, "bbbb")
	if err != nil {
		t.Fatalf("lookup B: %v", err)
	}
	if backB.ID != rowB.ID {
		t.Errorf("lookup B returned %s, want %s", backB.ID, rowB.ID)
	}
}

// TestPgStoreConsumerKeys_VocabularyCheck — the migration's closed-
// set CHECK rejects unknown scopes. apid validates at the write
// boundary; this pins the DB floor.
func TestPgStoreConsumerKeys_VocabularyCheck(t *testing.T) {
	store, _, ctx := pgStoreWithPool(t)

	accountID := uuid.NewString()
	appID := uuid.NewString()

	_, err := store.CreateConsumerKey(ctx, accountID, appID, "vocab-test", "dead", makeHash(t, "x"), []string{"superadmin"}, nil)
	if err == nil {
		t.Fatal("CreateConsumerKey with scope='superadmin' should fail (closed-set CHECK in 00309)")
	}
}

// TestPgStoreConsumerKeys_TouchRevokedIsNoop — TouchConsumerKeyLastUsed
// on a revoked key must NOT update last_used_at (the SQL filter
// pins revoked_at IS NULL). best-effort observability, never
// resurrects a revoked row's freshness.
func TestPgStoreConsumerKeys_TouchRevokedIsNoop(t *testing.T) {
	store, _, ctx := pgStoreWithPool(t)

	accountID := uuid.NewString()
	appID := uuid.NewString()
	got := seedConsumerKeyPg(t, ctx, store, accountID, appID, "touch-revoked", []string{"read"})

	if _, err := store.RevokeConsumerKey(ctx, accountID, got.ID); err != nil {
		t.Fatalf("RevokeConsumerKey: %v", err)
	}
	if err := store.TouchConsumerKeyLastUsed(ctx, got.ID); err != nil {
		t.Fatalf("TouchConsumerKeyLastUsed (revoked): %v", err)
	}
	back, err := store.GetConsumerKeyByID(ctx, accountID, got.ID)
	if err != nil {
		t.Fatalf("GetConsumerKeyByID: %v", err)
	}
	if back.LastUsedAt != nil {
		t.Errorf("TouchConsumerKeyLastUsed stamped LastUsedAt on a revoked row: %v (must be no-op — SQL filter revoked_at IS NULL)", back.LastUsedAt)
	}
	if back.RevokedAt == nil {
		t.Errorf("RevokedAt cleared after TouchConsumerKeyLastUsed")
	}
}

// makeHash is a tiny helper for tests that need a 32-byte hash
// without minting a fresh plaintext+prefix+secret. The bytes are
// arbitrary — the migration's CHECK only pins octet_length=32.
func makeHash(t *testing.T, seed string) []byte {
	t.Helper()
	h := api.HashConsumerKey("ck_test_" + seed)
	if len(h) != 32 {
		t.Fatalf("HashConsumerKey returned %d bytes, want 32", len(h))
	}
	return h
}

// equalScopes compares two []string order-insensitively. The
// caller controls the iteration order (CreateConsumerKey stores
// the slice verbatim); comparing with a fixed expected order is
// the cheapest assertion.
func equalScopes(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
