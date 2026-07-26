// Issue #279 PR A — Stripe webhook mapping for charge.refunded.
//
// The Refund gRPC seam is exercised end-to-end through the apid
// handler (cmd/apid/handlers_admin_credits_test.go) and the e2e
// suite (cmd/e2e/credit_e2e_test.go). The contract pinned here is
// the webhook → EventRefundProcessed mapping that the apid
// handler dispatches against, since that's the surface that turns
// a Stripe `charge.refunded` event into a `refund.processed` audit
// row.
package stripe_test

import (
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/billing"
	"github.com/onebox-faas/faas/pkg/billing/stripe"
	"github.com/onebox-faas/faas/pkg/state"
)

// TestVerifyWebhook_ChargeRefunded — a signed charge.refunded webhook
// maps to EventRefundProcessed and carries the integer cents + charge
// ID + currency we need for the audit row.
func TestVerifyWebhook_ChargeRefunded(t *testing.T) {
	t.Parallel()
	store := state.NewMemStore()
	c := stripe.NewClient(store, store, "sk_test_dummy", testSecret, discardLog())

	// 5000 cents = €50.00. Stripe uses millicents: 5000 * 10 = 50000.
	payload := []byte(`{
		"type": "charge.refunded",
		"data": {"object": {
			"id": "ch_test_123",
			"customer": "cus_test_alice",
			"amount_refunded": 50000,
			"currency": "eur",
			"refunded": true
		}}
	}`)
	headers := map[string]string{
		"Stripe-Signature": stripe.SignForTest(payload, testSecret, time.Now()),
	}
	ev, err := c.VerifyWebhook(payload, headers, 5*time.Minute)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if ev.Type != billing.EventRefundProcessed {
		t.Fatalf("Type = %v, want EventRefundProcessed", ev.Type)
	}
	if got := ev.Type.Name(); got != "refund_processed" {
		t.Errorf("Type.Name() = %q, want refund_processed", got)
	}
	if ev.ChargeID != "ch_test_123" {
		t.Errorf("ChargeID = %q, want ch_test_123", ev.ChargeID)
	}
	if ev.AmountCents != 5000 {
		t.Errorf("AmountCents = %d, want 5000 (50000 millicents → 5000 cents)", ev.AmountCents)
	}
	if ev.Currency != "eur" {
		t.Errorf("Currency = %q, want eur", ev.Currency)
	}
}
