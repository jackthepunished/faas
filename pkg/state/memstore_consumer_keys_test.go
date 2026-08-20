package state_test

// Memstore mirror of the pgstore consumer_keys tests (ADR-120 /
// issue #975 item #5). The memstore MUST agree with the pg path
// on every observable contract — a caller validated against
// in-memory storage ships an IDOR when moved to PG.
//
// Same pattern as memstore_cors_presets_test.go: every method
// exercised end-to-end through state.NewMemStore(), every IDOR
// pin surfaced, every CASCADE mirror verified.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

func TestMemStoreConsumerKeys_RoundTrip(t *testing.T) {
	st := state.NewMemStore()
	ctx := context.Background()

	accountID := uuid.NewString()
	appID := uuid.NewString()
	name := "mem-rt-" + uuid.NewString()[:8]

	plaintext, prefix, hash, err := api.GenerateConsumerKey()
	if err != nil {
		t.Fatalf("GenerateConsumerKey: %v", err)
	}
	if !api.ValidConsumerKeyFormat(plaintext) {
		t.Fatalf("freshly minted key failed format check: %q", plaintext)
	}

	// Create.
	expiresAt := time.Now().Add(365 * 24 * time.Hour).UTC()
	created, err := st.CreateConsumerKey(ctx, accountID, appID, name, prefix, hash, []string{"read", "write"}, &expiresAt)
	if err != nil {
		t.Fatalf("CreateConsumerKey: %v", err)
	}
	if created.ID == "" {
		t.Fatal("empty ID on freshly-created key")
	}

	// GetByID.
	back, err := st.GetConsumerKeyByID(ctx, accountID, created.ID)
	if err != nil {
		t.Fatalf("GetConsumerKeyByID: %v", err)
	}
	if back.ID != created.ID || back.Prefix != created.Prefix {
		t.Errorf("GetConsumerKeyByID round-trip mismatch")
	}

	// ConsumerKeyByAppAndPrefix.
	byPrefix, err := st.ConsumerKeyByAppAndPrefix(ctx, accountID, appID, prefix)
	if err != nil {
		t.Fatalf("ConsumerKeyByAppAndPrefix: %v", err)
	}
	if byPrefix.ID != created.ID {
		t.Errorf("byPrefix.ID = %s, want %s", byPrefix.ID, created.ID)
	}

	// ListForApp — should contain only this one key.
	list, err := st.ListConsumerKeysForApp(ctx, accountID, appID)
	if err != nil {
		t.Fatalf("ListConsumerKeysForApp: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListConsumerKeysForApp returned %d rows, want 1", len(list))
	}
	if list[0].ID != created.ID {
		t.Errorf("list[0].ID = %s, want %s", list[0].ID, created.ID)
	}

	// Touch.
	if err := st.TouchConsumerKeyLastUsed(ctx, created.ID); err != nil {
		t.Fatalf("TouchConsumerKeyLastUsed: %v", err)
	}
	touched, _ := st.GetConsumerKeyByID(ctx, accountID, created.ID)
	if touched.LastUsedAt == nil {
		t.Errorf("LastUsedAt still nil after touch")
	}

	// Revoke.
	revoked, err := st.RevokeConsumerKey(ctx, accountID, created.ID)
	if err != nil {
		t.Fatalf("RevokeConsumerKey: %v", err)
	}
	if revoked.RevokedAt == nil {
		t.Errorf("RevokedAt nil after revoke")
	}

	// Idempotent revoke.
	revoked2, err := st.RevokeConsumerKey(ctx, accountID, created.ID)
	if err != nil {
		t.Fatalf("RevokeConsumerKey idempotent: %v", err)
	}
	if !revoked.RevokedAt.Equal(*revoked2.RevokedAt) {
		t.Errorf("idempotent revoke changed RevokedAt: %v → %v", *revoked.RevokedAt, *revoked2.RevokedAt)
	}
}

func TestMemStoreConsumerKeys_IDOR(t *testing.T) {
	st := state.NewMemStore()
	ctx := context.Background()

	accountA := uuid.NewString()
	accountB := uuid.NewString()
	appA := uuid.NewString()

	_, prefix, hash, err := api.GenerateConsumerKey()
	if err != nil {
		t.Fatalf("GenerateConsumerKey: %v", err)
	}
	created, err := st.CreateConsumerKey(ctx, accountA, appA, "idor-mem", prefix, hash, []string{"read"}, nil)
	if err != nil {
		t.Fatalf("CreateConsumerKey: %v", err)
	}

	if _, err := st.GetConsumerKeyByID(ctx, accountB, created.ID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("GetConsumerKeyByID cross-account = %v, want ErrNotFound", err)
	}
	if _, err := st.ConsumerKeyByAppAndPrefix(ctx, accountB, appA, prefix); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("ConsumerKeyByAppAndPrefix cross-account = %v, want ErrNotFound", err)
	}
	if _, err := st.RevokeConsumerKey(ctx, accountB, created.ID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("RevokeConsumerKey cross-account = %v, want ErrNotFound", err)
	}

	listB, err := st.ListConsumerKeysForApp(ctx, accountB, appA)
	if err != nil {
		t.Fatalf("ListConsumerKeysForApp cross-account: %v", err)
	}
	if len(listB) != 0 {
		t.Errorf("ListConsumerKeysForApp cross-account returned %d rows, want 0", len(listB))
	}
}

// TestMemStoreConsumerKeys_NameUniqueness — the (account_id,
// app_id, name) UNIQUE permits the same name in different apps
// of the same account, but rejects duplicates within one
// (account, app). Mirrors pgstore.
func TestMemStoreConsumerKeys_NameUniqueness(t *testing.T) {
	st := state.NewMemStore()
	ctx := context.Background()

	accountID := uuid.NewString()
	appA := uuid.NewString()
	appB := uuid.NewString()
	name := "shared-" + uuid.NewString()[:8]

	hashX := memHash(t, "x")
	hashY := memHash(t, "y")

	if _, err := st.CreateConsumerKey(ctx, accountID, appA, name, "aaaa", hashX, []string{"read"}, nil); err != nil {
		t.Fatalf("CreateConsumerKey appA: %v", err)
	}
	if _, err := st.CreateConsumerKey(ctx, accountID, appB, name, "bbbb", hashY, []string{"read"}, nil); err != nil {
		t.Fatalf("CreateConsumerKey appB (same name, different app): %v (must succeed)", err)
	}
	// Same name, same app — must fail with ErrConflict.
	if _, err := st.CreateConsumerKey(ctx, accountID, appA, name, "cccc", memHash(t, "z"), []string{"read"}, nil); !errors.Is(err, state.ErrConflict) {
		t.Errorf("CreateConsumerKey duplicate in (account, app) = %v, want ErrConflict", err)
	}
}

// TestMemStoreConsumerKeys_EmptyGuards — empty inputs must be
// rejected at the Store boundary, NOT surface as SQL NULL /
// out-of-band errors. Mirrors pgstore.
func TestMemStoreConsumerKeys_EmptyGuards(t *testing.T) {
	st := state.NewMemStore()
	ctx := context.Background()

	hash := memHash(t, "x")
	if _, err := st.CreateConsumerKey(ctx, "", "a", "n", "p", hash, []string{"read"}, nil); err == nil {
		t.Error("CreateConsumerKey with empty accountID: expected error")
	}
	if _, err := st.CreateConsumerKey(ctx, "a", "", "n", "p", hash, []string{"read"}, nil); err == nil {
		t.Error("CreateConsumerKey with empty appID: expected error")
	}
	if _, err := st.CreateConsumerKey(ctx, "a", "b", "n", "p", hash, []string{}, nil); err == nil {
		t.Error("CreateConsumerKey with empty scopes: expected error (closed-set CHECK)")
	}
	if _, err := st.CreateConsumerKey(ctx, "a", "b", "n", "p", []byte("short"), []string{"read"}, nil); err == nil {
		t.Error("CreateConsumerKey with non-32-byte hash: expected error")
	}
	if _, err := st.GetConsumerKeyByID(ctx, "", "k"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("GetConsumerKeyByID empty accountID = %v, want ErrNotFound", err)
	}
	if _, err := st.ConsumerKeyByAppAndPrefix(ctx, "a", "b", ""); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("ConsumerKeyByAppAndPrefix empty prefix = %v, want ErrNotFound", err)
	}
	if _, err := st.RevokeConsumerKey(ctx, "", "k"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("RevokeConsumerKey empty accountID = %v, want ErrNotFound", err)
	}
}

// TestMemStoreConsumerKeys_Ordering — ListConsumerKeysForApp
// orders by created_at DESC, with ID as the stable tie-break.
// Mirrors pgstore.
func TestMemStoreConsumerKeys_Ordering(t *testing.T) {
	st := state.NewMemStore()
	ctx := context.Background()

	accountID := uuid.NewString()
	appID := uuid.NewString()

	// Create three keys with explicit timestamps so the test is
	// deterministic (CreateConsumerKey uses time.Now() but the
	// three calls land within microseconds, which is still ordered).
	var ids []string
	for i := 0; i < 3; i++ {
		_, prefix, hash, err := api.GenerateConsumerKey()
		if err != nil {
			t.Fatalf("GenerateConsumerKey[%d]: %v", i, err)
		}
		got, err := st.CreateConsumerKey(ctx, accountID, appID, "k-"+uuid.NewString()[:8], prefix, hash, []string{"read"}, nil)
		if err != nil {
			t.Fatalf("CreateConsumerKey[%d]: %v", i, err)
		}
		ids = append(ids, got.ID)
		// Spread timestamps apart.
		time.Sleep(2 * time.Millisecond)
	}

	list, err := st.ListConsumerKeysForApp(ctx, accountID, appID)
	if err != nil {
		t.Fatalf("ListConsumerKeysForApp: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("len(list) = %d, want 3", len(list))
	}
	// Newest first — last inserted should be list[0].
	if list[0].ID != ids[2] {
		t.Errorf("list[0].ID = %s, want %s (newest first)", list[0].ID, ids[2])
	}
	if list[2].ID != ids[0] {
		t.Errorf("list[2].ID = %s, want %s (oldest last)", list[2].ID, ids[0])
	}
}

// memHash is a tiny helper for tests that need a 32-byte hash
// without minting a fresh plaintext+prefix+secret. The bytes are
// arbitrary — the migration's CHECK only pins octet_length=32.
func memHash(t *testing.T, seed string) []byte {
	t.Helper()
	h := api.HashConsumerKey("ck_test_" + seed)
	if len(h) != 32 {
		t.Fatalf("HashConsumerKey returned %d bytes, want 32", len(h))
	}
	return h
}
