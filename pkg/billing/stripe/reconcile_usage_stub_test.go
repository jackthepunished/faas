// Stripe ReconcileUsage stub contract tests (ADR-049 §B.1).
//
// Pins the behaviour every code reader relies on:
//
//  1. The stub must NEVER return (0, nil). Returning (0, nil) for
//     a paying Stripe account would make the reconciler's drift
//     ratio = abs(local − 0) / max(local, 0) = 1.0, which pages
//     the BillingDrift alert on every paid account from the moment
//     this PR ships. The right "no drift signal yet" sentinel is
//     billing.ErrNotImplemented — the reconciler's
//     `errors.Is(err, ErrNotImplemented)` short-circuit skips
//     the gauge emission (pkg/billing/reconciler/reconciler.go).
//
//  2. Every input shape (no customer id, no subscription item,
//     populated account) must produce ErrNotImplemented. This is
//     the unit-test tripwire for PR #428 review blocker #2 — if a
//     future refactor reintroduces the (0, nil) path the test
//     fails at unit-test time, well before a mis-configured fleet
//     pages the on-call.

package stripe

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/billing"
	"github.com/onebox-faas/faas/pkg/state"
)

// TestReconcileUsage_StubNeverReturnsZeroNil is the tripwire: any
// (0, nil) return path from ReconcileUsage is a BillingDrift
// false-positive waiting to happen. The stub must return
// ErrNotImplemented until the SDK summation lands in a follow-up.
func TestReconcileUsage_StubNeverReturnsZeroNil(t *testing.T) {
	c := NewClient(state.NewMemStore(), state.NewMemStore(), "", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Now().UTC()
	ctx := context.Background()

	cases := []struct {
		name string
		acct state.Account
	}{
		{
			name: "empty account (no customer, no sub item)",
			acct: state.Account{ID: "acct_empty"},
		},
		{
			name: "customer only",
			acct: state.Account{ID: "acct_c", ProviderCustomerID: "cus_test123"},
		},
		{
			name: "sub item only",
			acct: state.Account{ID: "acct_s", StripeSubscriptionItem: "si_test456"},
		},
		{
			name: "populated paid account",
			acct: state.Account{
				ID:                     "acct_full",
				ProviderCustomerID:     "cus_test789",
				StripeSubscriptionItem: "si_test789",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pushed, err := c.ReconcileUsage(ctx, tc.acct, now.Add(-24*time.Hour), now)
			if err == nil {
				t.Fatalf("ReconcileUsage(%s) = (%d, nil) — this would page the BillingDrift alert on every paid account. Return billing.ErrNotImplemented until the SDK summation lands.", tc.name, pushed)
			}
			if pushed != 0 {
				t.Errorf("ReconcileUsage(%s) pushed = %d, want 0 (stub returns only the sentinel error)", tc.name, pushed)
			}
			if !errors.Is(err, billing.ErrNotImplemented) {
				t.Errorf("ReconcileUsage(%s) err = %v, want billing.ErrNotImplemented", tc.name, err)
			}
		})
	}
}
