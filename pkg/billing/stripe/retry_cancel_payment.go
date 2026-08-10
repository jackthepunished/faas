// pkg/billing/stripe/retry_cancel_payment.go — Stripe-side
// implementations of the three new Provider interface methods
// added in issue #242:
//
//   - RetryLatestCharge: stripe.Invoices.Pay against the latest
//     open invoice for the customer.
//   - CancelAtPeriodEnd: stripe.Subscriptions.Update on the
//     account's stripe_subscription_item.
//   - PaymentMethodSummary: stripe.PaymentMethods.List on the
//     account's customer.
//
// The CLI's `faas billing {retry,cancel,payment-method}` surfaces
// (issue #242) and the dunning email body at
// pkg/mail/account.go:107,150 all route through these three
// methods. Stripe-side mirror for the same surfaces on Paddle
// lives in pkg/billing/paddle/retry_cancel_payment.go.
package stripe

import (
	"context"
	"fmt"
	"time"

	"github.com/onebox-faas/faas/pkg/billing"
	"github.com/onebox-faas/faas/pkg/state"
	stripe "github.com/stripe/stripe-go"
	"github.com/stripe/stripe-go/client"
)

// requireAPI returns the typed *client.API or a wrapped
// `ErrNoAPIKey` when none is constructed. Mirrors the pre-SDK
// guard shape at usage.go::pushUsageRecordSDKSumWithID so all
// three new methods share the same nil-api -> friendly-error
// behaviour without per-method nil checks.
func (c *Client) requireAPI() (*client.API, error) {
	if c.api == nil {
		return nil, fmt.Errorf("%w (stripe-side retry/cancel/payment_method requires apiKey)",
			billing.ErrNoAPIKey)
	}
	return c.api, nil
}

// RetryLatestCharge implements billing.Provider.RetryLatestCharge
// on Stripe (issue #242). Walks the customer's open invoices in
// reverse chronological order, picks the latest, and calls
// `Invoices.Pay` with an Idempotency-Key derived from
// `acct.ID + "/retry/" + invoice.ID` so a flaky-network
// redelivery collapses to one Stripe-side attempt.
//
// Returns ErrNoOpenCharge (wrapped) when the customer has no open
// invoice — the apid handler renders that as 404 so the CLI prints
// a friendly "already in good standing" hint. Returns ErrNoAPIKey
// when the operator has not configured an API key.
func (c *Client) RetryLatestCharge(ctx context.Context, acct state.Account) (string, string, error) {
	if acct.ProviderCustomerID == "" {
		// Free plan / never-checked-out account.
		return "", "", fmt.Errorf("stripe: RetryLatestCharge: %w (account %s, no customer)",
			billing.ErrNoOpenCharge, acct.ID)
	}
	apiClient, err := c.requireAPI()
	if err != nil {
		return "", "", err
	}

	// List open invoices, newest first. status=open excludes paid +
	// uncollectible + void. Limit=10 is bounded because we only
	// care about the most recent attempt.
	params := &stripe.InvoiceListParams{
		Customer: stripe.String(acct.ProviderCustomerID),
		Status:   stripe.String(string(stripe.InvoiceStatusOpen)),
		ListParams: stripe.ListParams{
			Limit: stripe.Int64(10),
		},
	}
	it := apiClient.Invoices.List(params)
	var latest *stripe.Invoice
	for it.Next() {
		inv := it.Invoice()
		if latest == nil || inv.Created > latest.Created {
			latest = inv
		}
	}
	if err := it.Err(); err != nil {
		return "", "", fmt.Errorf("stripe: Invoices.List account %s: %w", acct.ID, err)
	}
	if latest == nil {
		return "", "", fmt.Errorf("%w (account %s, no open invoice)",
			billing.ErrNoOpenCharge, acct.ID)
	}

	// Pin the idempotency key on the per-invoice retry attempt.
	// Stripe honours Idempotency-Key across redeliveries within
	// 24h and returns the cached response. The key is unique per
	// (acct, invoice) so an attempt against invoice_2 cannot
	// collide with invoice_1.
	idem := acct.ID + "/retry/" + latest.ID
	payParams := &stripe.InvoicePayParams{}
	payParams.IdempotencyKey = stripe.String(idem)

	paid, err := apiClient.Invoices.Pay(latest.ID, payParams)
	if err != nil {
		return "", "", fmt.Errorf("stripe: Invoices.Pay invoice %s account %s: %w",
			latest.ID, acct.ID, err)
	}
	refID := ""
	if paid != nil && paid.PaymentIntent != nil {
		refID = string(paid.PaymentIntent.ID)
	}
	if paid == nil {
		return "", "", fmt.Errorf("stripe: Invoices.Pay invoice %s account %s: nil invoice",
			latest.ID, acct.ID)
	}
	return paid.ID, refID, nil
}

// CancelAtPeriodEnd implements billing.Provider.CancelAtPeriodEnd
// on Stripe (issue #242). Calls
// `stripe.Subscriptions.Update(cancel_at_period_end=true)` against
// the account's stripe_subscription_item and returns the
// `current_period_end` as the effective timestamp.
//
// Stripe's re-cancel of an already-cancelled subscription is
// idempotent and returns 200, so we do NOT surface
// ErrAlreadyCancelled on re-cancel. ErrAlreadyCancelled is returned
// only when the account has no stripe_subscription_item at all
// (Free plan / post-cancel / never-checked-out).
//
// The flag lives on Stripe-side; the dashboard reads
// `acct.StripeSubscriptionItem` and asks Stripe via the existing
// `/v1/subscriptions/{id}` endpoint on every render, so we never
// mirror `cancel_at_period_end` locally.
func (c *Client) CancelAtPeriodEnd(ctx context.Context, acct state.Account) (time.Time, error) {
	if acct.StripeSubscriptionItem == "" {
		// No active subscription = nothing to cancel. Render
		// as already-cancelled so the CLI's idempotent cancel
		// (a Free-plan user's first try) gets a friendly hint.
		return time.Time{}, fmt.Errorf("%w (account %s, no subscription)",
			billing.ErrAlreadyCancelled, acct.ID)
	}
	apiClient, err := c.requireAPI()
	if err != nil {
		return time.Time{}, err
	}
	params := &stripe.SubscriptionParams{
		CancelAtPeriodEnd: stripe.Bool(true),
	}
	sub, err := apiClient.Subscriptions.Update(acct.StripeSubscriptionItem, params)
	if err != nil {
		return time.Time{}, fmt.Errorf("stripe: Subscriptions.Update %s account %s: %w",
			acct.StripeSubscriptionItem, acct.ID, err)
	}
	if sub == nil || sub.CurrentPeriodEnd == 0 {
		// Stripe should always return current_period_end on a
		// paid subscription, but defend against a wire-format
		// drift. The CLI renders the zero as "your apps will
		// stop on the next cycle" rather than crashing.
		return time.Time{}, nil
	}
	return time.Unix(sub.CurrentPeriodEnd, 0).UTC(), nil
}

// PaymentMethodSummary implements billing.Provider.PaymentMethodSummary
// on Stripe (issue #242). Calls `stripe.PaymentMethods.List` against
// the account's customer and reduces the wire shape
// (brand, last4, exp_month, exp_year) to the
// pkg/billing.PaymentMethod internal type.
//
// A Free / no-card-on-file customer returns the zero
// PaymentMethod (zero brand + last4 + 0 exp fields). The CLI
// renders the zero as the "no payment method on file" CTA; the
// dashboard renders an "Add payment method" button. apid's
// handler omits the field on the JSON response in that case so
// the wire shape stays clean.
func (c *Client) PaymentMethodSummary(ctx context.Context, acct state.Account) (billing.PaymentMethod, error) {
	if acct.ProviderCustomerID == "" {
		// No customer = no card on file. Zero-value PaymentMethod.
		return billing.PaymentMethod{}, nil
	}
	apiClient, err := c.requireAPI()
	if err != nil {
		return billing.PaymentMethod{}, err
	}
	list := apiClient.PaymentMethods.List(&stripe.PaymentMethodListParams{
		Customer: stripe.String(acct.ProviderCustomerID),
		Type:     stripe.String("card"),
		ListParams: stripe.ListParams{
			Limit: stripe.Int64(1),
		},
	})
	if !list.Next() {
		// No card on file = the customer never finished
		// Stripe Checkout. Zero-value.
		if err := list.Err(); err != nil {
			return billing.PaymentMethod{}, fmt.Errorf("stripe: PaymentMethods.List account %s: %w",
				acct.ID, err)
		}
		return billing.PaymentMethod{}, nil
	}
	method := list.PaymentMethod()
	if method == nil || method.Card == nil {
		return billing.PaymentMethod{}, nil
	}
	return billing.PaymentMethod{
		Brand:    string(method.Card.Brand),
		Last4:    method.Card.Last4,
		ExpMonth: int(method.Card.ExpMonth),
		ExpYear:  int(method.Card.ExpYear),
	}, nil
}
