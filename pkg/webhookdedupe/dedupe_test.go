package webhookdedupe

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/state"
)

// TestCheckReplay_FirstDelivery_FreshAndRecords covers the happy
// path: empty table → CheckReplay returns nil AND records the row.
// The audit seam is downstream of CheckReplay; covered separately.
func TestCheckReplay_FirstDelivery_FreshAndRecords(t *testing.T) {
	s := state.NewMemStore()
	ctx := context.Background()

	if err := CheckReplay(ctx, s, ProviderGitHub, "delivery-abc-123"); err != nil {
		t.Fatalf("first delivery should be fresh; err=%v", err)
	}
	// Round-trip: a second CheckReplay within TTL should be rejected
	// (proves the row was recorded).
	err := CheckReplay(ctx, s, ProviderGitHub, "delivery-abc-123")
	if !IsReplay(err) {
		t.Fatalf("second delivery within TTL should be a replay; err=%v", err)
	}
}

// TestCheckReplay_SecondDeliveryWithinTTL_Rejected covers the
// security-critical branch: the same (provider, delivery_id) pair
// arriving twice in the TTL window returns *Replay (errors.Is
// matches state.ErrReplay).
func TestCheckReplay_SecondDeliveryWithinTTL_Rejected(t *testing.T) {
	s := state.NewMemStore()
	ctx := context.Background()

	if err := CheckReplay(ctx, s, ProviderGitHub, "delivery-abc-123"); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	err := CheckReplay(ctx, s, ProviderGitHub, "delivery-abc-123")
	if err == nil {
		t.Fatalf("second delivery within TTL should be rejected")
	}
	if !errors.Is(err, state.ErrReplay) {
		t.Errorf("errors.Is(err, state.ErrReplay) = false; got %v", err)
	}
	if !IsReplay(err) {
		t.Errorf("IsReplay(err) = false; got %v", err)
	}
	var replay *Replay
	if !errors.As(err, &replay) {
		t.Fatalf("errors.As(*Replay) should succeed; got %T", err)
	}
	if replay.Provider != ProviderGitHub || replay.DeliveryID != "delivery-abc-123" {
		t.Errorf("Replay payload wrong: %+v", replay)
	}
}

// TestCheckReplay_DeliveryAfterTTL_Fresh covers the TTL boundary:
// a delivery whose stored expires_at is older than the cutoff
// (computed as now-TTL inside CheckReplay) is treated as fresh
// and re-recorded. We pre-seed a row via RecordWebhookDelivery
// with a stale expires_at to exercise the path without sleeping.
func TestCheckReplay_DeliveryAfterTTL_Fresh(t *testing.T) {
	s := state.NewMemStore()
	ctx := context.Background()

	// Pre-seed a row that is already past the TTL window — the
	// MemStore's CheckWebhookReplay drops it inline (per
	// memstore.go's TTL semantics), so CheckReplay sees it as
	// fresh and refreshes expires_at.
	if err := s.RecordWebhookDelivery(ctx, ProviderGitHub, "stale", time.Now().Add(-2*TTL)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := CheckReplay(ctx, s, ProviderGitHub, "stale"); err != nil {
		t.Fatalf("delivery after TTL should be fresh; err=%v", err)
	}
}

// TestCheckReplay_DifferentProviders_Independent covers the PK
// shape: same delivery_id, different provider → both fresh
// (the (provider, delivery_id) PK on webhook_deliveries scopes
// rows per provider, not globally).
func TestCheckReplay_DifferentProviders_Independent(t *testing.T) {
	s := state.NewMemStore()
	ctx := context.Background()

	if err := CheckReplay(ctx, s, ProviderGitHub, "shared-id"); err != nil {
		t.Fatalf("github first delivery: %v", err)
	}
	if err := CheckReplay(ctx, s, ProviderStripe, "shared-id"); err != nil {
		t.Fatalf("stripe first delivery (different provider, same id) should be fresh; err=%v", err)
	}
	if err := CheckReplay(ctx, s, ProviderPaddle, "shared-id"); err != nil {
		t.Fatalf("paddle first delivery (different provider, same id) should be fresh; err=%v", err)
	}
}

// TestCheckReplay_DifferentDeliveryIDs_Independent covers the
// second axis: same provider, different delivery_id → both fresh.
// (Issue #294 acceptance criterion 4 is a happy-path test in
// gatewayd; this one is the unit-level pair.)
func TestCheckReplay_DifferentDeliveryIDs_Independent(t *testing.T) {
	s := state.NewMemStore()
	ctx := context.Background()

	if err := CheckReplay(ctx, s, ProviderGitHub, "delivery-1"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := CheckReplay(ctx, s, ProviderGitHub, "delivery-2"); err != nil {
		t.Fatalf("different delivery_id should be fresh; err=%v", err)
	}
}

// TestSweep_DropsExpiredRows covers the sweep's contract: rows
// older than the cutoff are removed; rows newer than the cutoff
// remain.
func TestSweep_DropsExpiredRows(t *testing.T) {
	s := state.NewMemStore()
	ctx := context.Background()
	now := time.Now()

	if err := s.RecordWebhookDelivery(ctx, ProviderGitHub, "expired", now.Add(-time.Hour)); err != nil {
		t.Fatalf("seed expired: %v", err)
	}
	if err := s.RecordWebhookDelivery(ctx, ProviderGitHub, "fresh", now.Add(time.Hour)); err != nil {
		t.Fatalf("seed fresh: %v", err)
	}

	swept, err := Sweep(ctx, s, now)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if swept != 1 {
		t.Errorf("swept = %d, want 1", swept)
	}
	// After the sweep, the expired row's next CheckReplay is fresh
	// (MemStore's CheckWebhookReplay drops it inline); the fresh
	// row's next CheckReplay is a replay.
	if err := CheckReplay(ctx, s, ProviderGitHub, "expired"); err != nil {
		t.Errorf("after-sweep, expired should be fresh; got err=%v", err)
	}
	if err := CheckReplay(ctx, s, ProviderGitHub, "fresh"); !IsReplay(err) {
		t.Errorf("after-sweep, fresh should still be a replay; got err=%v", err)
	}
}

// TestErrReplay_AliasesStateSentinel pins the wrapper contract:
// webhookdedupe.ErrReplay IS state.ErrReplay (same value, not a
// copy), so errors.Is from either side matches.
func TestErrReplay_AliasesStateSentinel(t *testing.T) {
	if ErrReplay != state.ErrReplay {
		t.Errorf("webhookdedupe.ErrReplay (%v) should be the same value as state.ErrReplay (%v)", ErrReplay, state.ErrReplay)
	}
}

// TestReplay_TypeAssertions covers the typed error wrapper for
// callers that want both the bool (IsReplay) and the typed payload
// (errors.As *Replay).
func TestReplay_TypeAssertions(t *testing.T) {
	inner := &Replay{Provider: ProviderStripe, DeliveryID: "evt_test_123"}
	wrapped := state.ErrReplay // bare sentinel — covers the IsReplay(err) == true branch on bare sentinels too.

	if !IsReplay(inner) {
		t.Errorf("IsReplay on *Replay should be true")
	}
	if !errors.Is(inner, state.ErrReplay) {
		t.Errorf("errors.Is(*Replay, state.ErrReplay) should be true")
	}
	if !errors.Is(inner, ErrReplay) {
		t.Errorf("errors.Is(*Replay, webhookdedupe.ErrReplay) should be true")
	}
	if !IsReplay(wrapped) {
		t.Errorf("IsReplay on bare state.ErrReplay should be true too — covers the sentinel-only branch")
	}
}
