// Unit tests for the in-memory Leaser[*memLeaseRecord] implementation.
// Exercises Acquire / Renew / Release / Lookup / ReapExpired +
// classification of ErrLeaseNotFound / ErrLeaseExpired /
// ErrLeaseHeldByOther. Does not require a DB; the production path is
// pgx-based and lives in lease_pg.go (covered by metal-tagged tests).
package sched

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// memLeaserClock returns a deterministic clock function so lease
// expiry tests don't sleep.
func memLeaserClock(t time.Time) func() time.Time {
	var mu sync.Mutex
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return t
	}
}

func TestMemLeaser_Acquire_Renew_Release(t *testing.T) {
	clock := memLeaserClock(time.Unix(1_700_000_000, 0))
	l := NewMemLeaser(clock)
	ctx := context.Background()

	tok, rec, err := l.Acquire(ctx, "run-1\x000", LeasePolicy{TTL: 60 * time.Second}, "node-A")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if tok == "" {
		t.Fatal("Acquire returned empty token")
	}
	if rec == nil || rec.key != "run-1\x000" || rec.ownerID != "node-A" {
		t.Fatalf("Acquire returned unexpected record: %+v", rec)
	}
	if got := l.Size(); got != 1 {
		t.Fatalf("Size after Acquire: want 1, got %d", got)
	}

	// Renew from the correct owner.
	if err := l.Renew(ctx, tok, "node-A", 120*time.Second); err != nil {
		t.Fatalf("Renew: %v", err)
	}

	// Lookup should still see it.
	key, exp, owner, ok, err := l.Lookup(ctx, tok)
	if err != nil || !ok {
		t.Fatalf("Lookup after Renew: ok=%v err=%v", ok, err)
	}
	if key != "run-1\x000" || owner != "node-A" || !exp.After(clock()) {
		t.Fatalf("Lookup returned unexpected: key=%s owner=%s exp=%s", key, owner, exp)
	}

	// Release from the correct owner.
	if err := l.Release(ctx, tok, "node-A"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := l.Size(); got != 0 {
		t.Fatalf("Size after Release: want 0, got %d", got)
	}

	// Second Release is idempotent → ErrLeaseNotFound.
	if err := l.Release(ctx, tok, "node-A"); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("Second Release: want ErrLeaseNotFound, got %v", err)
	}
}

func TestMemLeaser_RenewByWrongOwner(t *testing.T) {
	clock := memLeaserClock(time.Unix(1_700_000_000, 0))
	l := NewMemLeaser(clock)
	ctx := context.Background()

	tok, _, err := l.Acquire(ctx, "run-2\x007", LeasePolicy{TTL: 60 * time.Second}, "node-A")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// Renew from a different owner → ErrLeaseHeldByOther.
	if err := l.Renew(ctx, tok, "node-B", 30*time.Second); !errors.Is(err, ErrLeaseHeldByOther) {
		t.Fatalf("Renew wrong owner: want ErrLeaseHeldByOther, got %v", err)
	}

	// The original lease should still be valid.
	if _, _, _, ok, _ := l.Lookup(ctx, tok); !ok {
		t.Fatal("Lease should still be valid after wrong-owner renew attempt")
	}
}

func TestMemLeaser_ReapExpired(t *testing.T) {
	clock := memLeaserClock(time.Unix(1_700_000_000, 0))
	l := NewMemLeaser(clock)
	ctx := context.Background()

	tokA, _, _ := l.Acquire(ctx, "k-a", LeasePolicy{TTL: 30 * time.Second}, "node-A")
	tokB, _, _ := l.Acquire(ctx, "k-b", LeasePolicy{TTL: 90 * time.Second}, "node-A")

	// Advance "now" past tokA's expiry but before tokB's.
	l.now = func() time.Time { return time.Unix(1_700_000_040, 0) }

	expired := l.ReapExpired()
	if len(expired) != 1 || expired[0] != tokA {
		t.Fatalf("ReapExpired: want [%s], got %v", tokA, expired)
	}

	// Lookup on the expired token should return ok=false (reaper took it).
	if _, _, _, ok, _ := l.Lookup(ctx, tokA); ok {
		t.Fatal("Expired token should not be visible after ReapExpired sweep")
	}
	if _, _, _, ok, _ := l.Lookup(ctx, tokB); !ok {
		t.Fatal("Unexpired token should still be visible")
	}
}

func TestMemLeaser_AcquireRejectsBadPolicy(t *testing.T) {
	l := NewMemLeaser(nil)
	ctx := context.Background()

	cases := []struct {
		name   string
		policy LeasePolicy
	}{
		{"zero TTL", LeasePolicy{TTL: 0}},
		{"negative TTL", LeasePolicy{TTL: -1 * time.Second}},
		{"negative MaxAttempts", LeasePolicy{TTL: time.Second, MaxAttempts: -1}},
		{"MaxDuration < TTL", LeasePolicy{TTL: 60 * time.Second, MaxDuration: 30 * time.Second}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := l.Acquire(ctx, "k", tc.policy, "node")
			if !errors.Is(err, ErrInvalidLeasePolicy) {
				t.Fatalf("want ErrInvalidLeasePolicy, got %v", err)
			}
		})
	}
}

func TestMemLeaser_AcquireReacquireFromSameOwner(t *testing.T) {
	clock := memLeaserClock(time.Unix(1_700_000_000, 0))
	l := NewMemLeaser(clock)
	ctx := context.Background()

	tok1, _, err := l.Acquire(ctx, "k-shared", LeasePolicy{TTL: 30 * time.Second}, "node-A")
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	tok2, _, err := l.Acquire(ctx, "k-shared", LeasePolicy{TTL: 60 * time.Second}, "node-A")
	if err != nil {
		t.Fatalf("re-Acquire from same owner: %v", err)
	}
	if tok1 != tok2 {
		t.Fatalf("re-Acquire should refresh the same token; got %s vs %s", tok1, tok2)
	}
	if got := l.Size(); got != 1 {
		t.Fatalf("Size: want 1, got %d", got)
	}
}

func TestMemLeaser_AcquireRefusesDifferentOwner(t *testing.T) {
	clock := memLeaserClock(time.Unix(1_700_000_000, 0))
	l := NewMemLeaser(clock)
	ctx := context.Background()

	_, _, err := l.Acquire(ctx, "k-stolen", LeasePolicy{TTL: 30 * time.Second}, "node-A")
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	_, _, err = l.Acquire(ctx, "k-stolen", LeasePolicy{TTL: 30 * time.Second}, "node-B")
	if !errors.Is(err, ErrLeaseHeldByOther) {
		t.Fatalf("re-Acquire from different owner: want ErrLeaseHeldByOther, got %v", err)
	}
}

func TestMemLeaser_TokensAreUnique(t *testing.T) {
	l := NewMemLeaser(nil)
	ctx := context.Background()
	seen := make(map[LeaseToken]bool)
	for i := 0; i < 1000; i++ {
		key := []byte("k-")
		key = append(key, byte('0'+(i/100)%10), byte('0'+(i/10)%10), byte('0'+i%10))
		tok, _, err := l.Acquire(ctx, string(key), LeasePolicy{TTL: time.Second}, "node")
		if err != nil {
			t.Fatalf("Acquire[%d]: %v", i, err)
		}
		if seen[tok] {
			t.Fatalf("duplicate token at iteration %d: %s", i, tok)
		}
		seen[tok] = true
	}
}