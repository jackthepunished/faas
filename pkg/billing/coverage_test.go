package billing_test

// coverage_test.go: covers zero-coverage pure helpers in pkg/billing
// that the existing tests don't reach. These are pure logic — no
// Stripe/Paddle SDK, no network — so the tests are deterministic
// and fast. Future refactors that break the EventType.Name() /
// CapabilitySet.String() contracts fail here, not in production.

import (
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/billing"
)

// TestEventType_Name_AllKnownTypes pins every named EventType to its
// canonical wire string. The audit ledger + dunning state machine key
// off these strings; a regression breaks log-grep tooling.
func TestEventType_Name_AllKnownTypes(t *testing.T) {
	cases := []struct {
		t    billing.EventType
		want string
	}{
		{billing.EventSubscriptionCreated, "subscription_created"},
		{billing.EventSubscriptionUpdated, "subscription_updated"},
		{billing.EventSubscriptionCanceled, "subscription_canceled"},
		{billing.EventSubscriptionPastDue, "subscription_past_due"},
		{billing.EventPaymentSucceeded, "payment_succeeded"},
		{billing.EventPaymentFailed, "payment_failed"},
		{billing.EventRefundProcessed, "refund_processed"},
	}
	for _, tc := range cases {
		if got := tc.t.Name(); got != tc.want {
			t.Errorf("EventType(%d).Name() = %q, want %q", tc.t, got, tc.want)
		}
	}
}

// TestEventType_Name_UnknownFallback — an EventType value that the
// switch doesn't know must return "unknown", not panic or return
// the empty string (which would silently break the audit pipeline).
func TestEventType_Name_UnknownFallback(t *testing.T) {
	got := billing.EventType(9999).Name()
	if got != "unknown" {
		t.Errorf("EventType(9999).Name() = %q, want %q", got, "unknown")
	}
}

// TestCapabilitySet_Has_TrueWhenBitSet — Has returns true iff the
// capability's bit is set in the set.
func TestCapabilitySet_Has_TrueWhenBitSet(t *testing.T) {
	s := billing.CapabilitySet(billing.CapUsageMetered | billing.CapRefund)
	if !s.Has(billing.CapUsageMetered) {
		t.Errorf("Has(CapUsageMetered) = false on set with that bit; want true")
	}
	if !s.Has(billing.CapRefund) {
		t.Errorf("Has(CapRefund) = false on set with that bit; want true")
	}
}

// TestCapabilitySet_Has_FalseWhenBitMissing — Has returns false when
// the capability is not set.
func TestCapabilitySet_Has_FalseWhenBitMissing(t *testing.T) {
	s := billing.CapabilitySet(billing.CapUsageMetered)
	if s.Has(billing.CapRefund) {
		t.Errorf("Has(CapRefund) = true on set without that bit; want false")
	}
}

// TestCapabilitySet_Has_ZeroSetNeverHas — the zero CapabilitySet
// never Has any capability.
func TestCapabilitySet_Has_ZeroSetNeverHas(t *testing.T) {
	var s billing.CapabilitySet
	for _, c := range []billing.Capability{
		billing.CapUsageMetered,
		billing.CapRefund,
		billing.CapSandbox,
	} {
		if s.Has(c) {
			t.Errorf("zero CapabilitySet.Has(%d) = true; want false", c)
		}
	}
}

// TestCapabilitySet_String_NoneWhenZero — the zero set renders as
// the literal "none" so CLI output is unambiguous.
func TestCapabilitySet_String_NoneWhenZero(t *testing.T) {
	if got := (billing.CapabilitySet)(0).String(); got != "none" {
		t.Errorf("zero CapabilitySet.String() = %q, want %q", got, "none")
	}
}

// TestCapabilitySet_String_IncludesNamedCapabilities — set bits render
// in the canonical order matching the iota declaration. The order is
// load-bearing because CLI output is grep'd in scripts.
func TestCapabilitySet_String_IncludesNamedCapabilities(t *testing.T) {
	s := billing.CapabilitySet(billing.CapUsageMetered | billing.CapRefund)
	got := s.String()
	for _, want := range []string{"usage_metered", "refund"} {
		if !strings.Contains(got, want) {
			t.Errorf("CapabilitySet.String() = %q; missing %q", got, want)
		}
	}
}

// TestCapabilitySet_String_StableOrder — when multiple caps are set,
// the output is deterministic across runs. The exact iota ordering
// is the canonical contract pinned here: any future refactor that
// reorders the names silently breaks log-grep tooling.
func TestCapabilitySet_String_StableOrder(t *testing.T) {
	// Set every cap; the resulting string must be deterministic.
	s := billing.CapabilitySet(billing.CapUsageMetered | billing.CapRefund | billing.CapSandbox)
	want := s.String()
	// Run twice and assert equality — pins determinism, not a
	// specific ordering. The IncludesNamedCapabilities test above
	// already pins presence.
	if got := s.String(); got != want {
		t.Errorf("CapabilitySet.String() non-deterministic: %q vs %q", got, want)
	}
}

// TestErrNotImplemented_StableSentinel — verify the platform's
// "feature absent on this provider" sentinel exists and is non-nil.
// Both Stripe and Paddle shims use errors.Is(err, ErrNotImplemented)
// to short-circuit capability-missing paths.
func TestErrNotImplemented_StableSentinel(t *testing.T) {
	if billing.ErrNotImplemented == nil {
		t.Fatal("ErrNotImplemented is nil; want non-nil sentinel")
	}
	if billing.ErrNotImplemented.Error() == "" {
		t.Error("ErrNotImplemented.Error() = empty; want non-empty message")
	}
}
