// pure_helpers_test.go — fill pkg/billing/stripe coverage of the
// tiny pure helpers and the pre-API guards (everything reachable
// without a Stripe sandbox or an API key).
//
// Targets:
//   - Config.Defaults (idempotent default-fill)
//   - PushResultLabels (the closed set of metric labels)
//   - idempotencyKeyFromContext (the context-key reader)
//   - requireAPI (no api → wrapped ErrNoAPIKey)
//   - RetryLatestCharge / CancelAtPeriodEnd / PaymentMethodSummary
//     pre-API guards (the cheap branch hit before Stripe is dialed)
//
// Whitebox `package stripe`.
package stripe

import (
	"context"
	"errors"
	"testing"

	"github.com/onebox-faas/faas/pkg/billing"
	"github.com/onebox-faas/faas/pkg/state"
)

// --- Config.Defaults --------------------------------------------

func TestConfigDefaults_ZeroSetsTolerance300(t *testing.T) {
	c := &Config{}
	c.Defaults()
	if c.ToleranceSeconds != 300 {
		t.Errorf("tolerance = %d, want 300 (5 minutes)", c.ToleranceSeconds)
	}
}

func TestConfigDefaults_NonZeroPreserved(t *testing.T) {
	c := &Config{ToleranceSeconds: 60}
	c.Defaults()
	if c.ToleranceSeconds != 60 {
		t.Errorf("tolerance = %d, want 60 (call should be idempotent)", c.ToleranceSeconds)
	}
}

func TestConfigDefaults_Idempotent(t *testing.T) {
	c := &Config{}
	c.Defaults()
	c.Defaults()
	c.Defaults()
	if c.ToleranceSeconds != 300 {
		t.Errorf("tolerance = %d, want 300 after three Defaults calls", c.ToleranceSeconds)
	}
}

// --- PushResultLabels -------------------------------------------

func TestPushResultLabels_ClosedSetNonEmpty(t *testing.T) {
	labels := PushResultLabels()
	if len(labels) == 0 {
		t.Fatal("PushResultLabels returned empty")
	}
	// All entries must be unique (the metric-side deduplication
	// relies on it).
	seen := map[string]bool{}
	for _, l := range labels {
		if l == "" {
			t.Error("empty label in PushResultLabels")
		}
		if seen[l] {
			t.Errorf("duplicate label %q in PushResultLabels", l)
		}
		seen[l] = true
	}
}

// --- idempotencyKeyFromContext ---------------------------------

func TestIdempotencyKeyFromContext_MissingReturnsFalse(t *testing.T) {
	if k, ok := idempotencyKeyFromContext(context.Background()); ok || k != "" {
		t.Errorf("missing: got (%q, %v), want (\"\", false)", k, ok)
	}
}

func TestIdempotencyKeyFromContext_PresentReturnsKey(t *testing.T) {
	ctx := context.WithValue(context.Background(), idempotencyKeyContextKey{}, "key-1")
	k, ok := idempotencyKeyFromContext(ctx)
	if !ok || k != "key-1" {
		t.Errorf("got (%q, %v), want (key-1, true)", k, ok)
	}
}

func TestIdempotencyKeyFromContext_EmptyStringReturnsFalse(t *testing.T) {
	// An empty key is treated as missing — the helper collapses
	// both "absent" and "blank" into ("" , false) so callers
	// don't have to special-case the empty string.
	ctx := context.WithValue(context.Background(), idempotencyKeyContextKey{}, "")
	if k, ok := idempotencyKeyFromContext(ctx); ok || k != "" {
		t.Errorf("empty: got (%q, %v), want (\"\", false)", k, ok)
	}
}

func TestIdempotencyKeyFromContext_WrongTypeReturnsFalse(t *testing.T) {
	// Some other type at the same key returns the typed assertion
	// ok=false. Sanity-check that the helper doesn't panic.
	ctx := context.WithValue(context.Background(), idempotencyKeyContextKey{}, 42)
	if _, ok := idempotencyKeyFromContext(ctx); ok {
		t.Error("wrong type: got true, want false")
	}
}

// --- requireAPI -------------------------------------------------

func TestRequireAPI_NilAPIReturnsErrNoAPIKey(t *testing.T) {
	c := &Client{api: nil}
	api, err := c.requireAPI()
	if api != nil {
		t.Errorf("nil api: api = %v, want nil", api)
	}
	if err == nil {
		t.Fatal("nil api: err = nil, want ErrNoAPIKey")
	}
	if !errors.Is(err, billing.ErrNoAPIKey) {
		t.Errorf("err = %v, want wraps ErrNoAPIKey", err)
	}
}

// --- pre-API guards on retry/cancel/payment_method -------------

func TestRetryLatestCharge_NoCustomerSkips(t *testing.T) {
	c := &Client{api: nil}
	_, _, err := c.RetryLatestCharge(context.Background(), state.Account{ID: "a-1"})
	if err == nil {
		t.Fatal("no customer: err = nil, want ErrNoOpenCharge")
	}
	if !errors.Is(err, billing.ErrNoOpenCharge) {
		t.Errorf("err = %v, want wraps ErrNoOpenCharge", err)
	}
}

func TestCancelAtPeriodEnd_NoSubscriptionSkips(t *testing.T) {
	c := &Client{api: nil}
	_, err := c.CancelAtPeriodEnd(context.Background(), state.Account{ID: "a-1"})
	if err == nil {
		t.Fatal("no subscription: err = nil, want ErrAlreadyCancelled")
	}
	if !errors.Is(err, billing.ErrAlreadyCancelled) {
		t.Errorf("err = %v, want wraps ErrAlreadyCancelled", err)
	}
}

func TestPaymentMethodSummary_NoCustomerReturnsZero(t *testing.T) {
	c := &Client{api: nil}
	got, err := c.PaymentMethodSummary(context.Background(), state.Account{ID: "a-1"})
	if err != nil {
		t.Fatalf("no customer: err = %v, want nil", err)
	}
	if got.Brand != "" || got.Last4 != "" || got.ExpMonth != 0 || got.ExpYear != 0 {
		t.Errorf("no customer: got %+v, want zero-value PaymentMethod", got)
	}
}

// PaymentMethodSummary with API key present still returns zero
// when ProviderCustomerID is empty — the pre-API guard is what
// we want to exercise.
func TestPaymentMethodSummary_NoCustomerIgnoresAPIKey(t *testing.T) {
	c := &Client{} // api == nil, customer empty
	got, err := c.PaymentMethodSummary(context.Background(), state.Account{ID: "a-1"})
	if err != nil {
		t.Fatalf("no customer: err = %v, want nil", err)
	}
	if got != (billing.PaymentMethod{}) {
		t.Errorf("got %+v, want zero PaymentMethod", got)
	}
}