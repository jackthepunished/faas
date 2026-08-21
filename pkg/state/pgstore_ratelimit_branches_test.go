// pgstore_ratelimit_branches_test.go — branch coverage for the
// non-DB branches of pkg/state/pgstore_ratelimit.go.
//
// The DB-bound surface (pool.QueryRow, INSERT ... ON CONFLICT
// RETURNING, advisory locks) is gated by the central-ratelimit
// integration tests in pkg/gateway that need a real cluster.
// What this file pins — the only branches reachable without DB —
// is the defensive fallback in ConsumeToken:
//
//   - rps(plan) returns (0, false) → unknown plan → "behave
//     like the noop backend (always admit)"
//   - rps(plan) returns (rps, true) but rps <= 0 → zero rps →
//     "behave like the noop backend (always admit)"
//
// Both branches share the same return: (0, true, nil) — the
// limiter sees a successful consume with 0 tokens remaining
// (caller admits; the in-process bucket owns the truth).
//
// Whitebox test (package state) matching the pgstore_*_test.go
// convention.
package state

import (
	"context"
	"testing"
)

// TestPGRateLimitBackend_ConsumeToken_UnknownPlan pins the
// `b.rps(plan) → (0, false)` fallback. A stale plan string
// (cluster-upgrade scenario: a replica hasn't picked up the
// new RateLimitRPS map yet) MUST degrade soft, not panic or
// return a non-nil err that would silently shut the limiter
// down.
func TestPGRateLimitBackend_ConsumeToken_UnknownPlan(t *testing.T) {
	b := &PGRateLimitBackend{pool: nil, rps: func(plan string) (float64, bool) {
		return 0, false
	}}

	remaining, ok, err := b.ConsumeToken(context.Background(),
		"per-app", "subject-1", "plan-XYZ")

	if err != nil {
		t.Fatalf("ConsumeToken(unknown plan): want nil err (degrade soft), got %v", err)
	}
	if !ok {
		t.Errorf("ConsumeToken(unknown plan): ok = false, want true (noop fallback admits)")
	}
	if remaining != 0 {
		t.Errorf("ConsumeToken(unknown plan): remaining = %d, want 0", remaining)
	}
}

// TestPGRateLimitBackend_ConsumeToken_ZeroRps pins the
// `rps <= 0` branch. A plan whose rps drops to 0 (Plan
// deprecation / sunset window) MUST degrade soft, NOT error
// out. The fall-through is the same (0, true, nil) — the
// limiter falls back to the noop behavior.
func TestPGRateLimitBackend_ConsumeToken_ZeroRps(t *testing.T) {
	b := &PGRateLimitBackend{pool: nil, rps: func(plan string) (float64, bool) {
		return 0, true
	}}

	remaining, ok, err := b.ConsumeToken(context.Background(),
		"per-app", "subject-2", "plan-sunset")

	if err != nil {
		t.Fatalf("ConsumeToken(zero rps): want nil err (degrade soft), got %v", err)
	}
	if !ok {
		t.Errorf("ConsumeToken(zero rps): ok = false, want true")
	}
	if remaining != 0 {
		t.Errorf("ConsumeToken(zero rps): remaining = %d, want 0", remaining)
	}
}

// TestPGRateLimitBackend_ConsumeToken_NegativeRps pins the
// "rps is unexpectedly negative" branch. A buggy caller that
// passes a negative rps must NOT cause the SQL to run with
// negative values (would corrupt the counter). Degrade soft,
// same as zero / unknown.
func TestPGRateLimitBackend_ConsumeToken_NegativeRps(t *testing.T) {
	b := &PGRateLimitBackend{pool: nil, rps: func(plan string) (float64, bool) {
		return -1.5, true
	}}

	remaining, ok, err := b.ConsumeToken(context.Background(),
		"per-app", "subject-3", "plan-buggy")

	if err != nil {
		t.Fatalf("ConsumeToken(negative rps): want nil err, got %v", err)
	}
	if !ok {
		t.Errorf("ConsumeToken(negative rps): ok = false, want true")
	}
	if remaining != 0 {
		t.Errorf("ConsumeToken(negative rps): remaining = %d, want 0", remaining)
	}
}

// TestPGRateLimitBackend_InvalidateNoOp pins the documentation
// contract for Invalidate: it's intentionally a no-op for the
// Postgres-backed backend (Postgres IS the shared state). The
// Limiter's local-LRU invalidation is owned by the Limiter
// itself. The Invalidate signature is here only to satisfy
// the CentralBackend interface.
func TestPGRateLimitBackend_InvalidateNoOp(t *testing.T) {
	b := &PGRateLimitBackend{pool: nil, rps: nil}
	// Should not panic, should not return.
	b.Invalidate("per-app", "subject", "plan")
	b.Invalidate("", "", "")
}

// TestPGRateLimitBackend_NewStoresFields pins the constructor:
// pool + rps closure are retained on the struct, and the rps
// closure is callable through the returned pointer.
func TestPGRateLimitBackend_NewStoresFields(t *testing.T) {
	called := false
	rps := func(plan string) (float64, bool) {
		called = true
		return 5.0, true
	}
	b := NewPGRateLimitBackend(nil, rps)
	if b == nil {
		t.Fatal("NewPGRateLimitBackend returned nil")
	}
	if b.pool != nil {
		t.Error("pool should be the nil we passed")
	}
	// Exercise the closure through b.rps.
	_, _ = b.rps("plan")
	if !called {
		t.Error("rps closure was stored but not callable from the struct (closure dropped?)")
	}
}
