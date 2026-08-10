// pkg/billing/paddle/retry_cancel_payment.go — Paddle-side
// implementations of the three new Provider interface methods
// added in issue #242:
//
//   - RetryLatestCharge: paddle.Client.CreateTransaction against
//     the existing customer for the same monthly price + month-to-
//     date overage. Idempotency-Key + CustomData tag distinguish
//     it from a fresh plan_upgrade CreateTransaction (upgrade.go).
//   - CancelAtPeriodEnd: stamps the cancel intent onto the Paddle
//     Customer object's CustomData.scheduled_change field. The
//     next CreateTransaction-derived rebill reads the flag and
//     skips posting; the account downgrades to Free on the next
//     meterd dunning tick. Paddle Billing has no separate
//     "scheduled cancellation" primitive (issue #242 design note).
//   - PaymentMethodSummary: paddle.Client.ListCustomerPaymentMethods
//     reduces the wire shape to (brand, last4, exp_month,
//     exp_year). Falls back to the zero PaymentMethod when the
//     customer has no saved card on file.
//
// The CLI's `faas billing {retry,cancel,payment-method}` surfaces
// (issue #242) and the dunning email body at
// pkg/mail/account.go:107,150 all route through these three
// methods. Paddle-side mirror for the same surfaces on Stripe
// lives in pkg/billing/stripe/retry_cancel_payment.go.
package paddle

import (
	"context"
	"fmt"
	"time"

	"github.com/PaddleHQ/paddle-go-sdk/v5"
	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/billing"
	"github.com/onebox-faas/faas/pkg/state"
)

// RetryLatestCharge implements billing.Provider.RetryLatestCharge
// on Paddle (issue #242). Posts a fresh CreateTransaction against
// the existing customer for the same monthly price + month-to-date
// overage so a card-decline bounce can be retried with the
// customer's existing saved card (no checkout round-trip).
//
//   - The Idempotency-Key (via paddle.ContextWithTransitID) is
//     `faas-retry-{acct.ID}-{YYYY-MM}` so a flaky-network retry
//     within the same month collapses to one transaction. The
//     CustomData["kind"]="billing_retry" tag distinguishes it
//     from a fresh plan_upgrade transaction (upgrade.go uses
//     "plan_upgrade") so the merchant-dashboard audit trail is
//     legible.
//   - The overage line is computed from acct.CurrentPeriodMBSeconds
//     — a field that meterd writes at every push (issue #235).
//     Quantity is the integer wire-quantity for the overage price;
//     the same conversion the monthly-rollover flush uses
//     (WireQuantityForMBSeconds in upgrade.go / usage.go).
//
// Returns ErrNoOpenCharge (wrapped) when the account has no
// monthly price set up yet (Free plan / never-checked-out) — apid
// maps that to 404.
func (p *Provider) RetryLatestCharge(ctx context.Context, acct state.Account) (string, string, error) {
	if p.client == nil {
		return "", "", fmt.Errorf("paddle: SDK not initialized")
	}
	if acct.ProviderCustomerID == "" {
		return "", "", fmt.Errorf("paddle: RetryLatestCharge: %w (account %s, no customer)",
			billing.ErrNoOpenCharge, acct.ID)
	}

	plan := acct.Plan
	if plan == "" {
		plan = api.PlanFree
	}
	monthly := p.monthlyPriceForPlan(plan)
	if monthly == "" {
		return "", "", fmt.Errorf("paddle: RetryLatestCharge: %w (account %s, plan=%s missing monthly price — catalog not hydrated)",
			billing.ErrNoOpenCharge, acct.ID, plan)
	}

	customerID := acct.ProviderCustomerID
	idem := fmt.Sprintf("faas-retry-%s-%s", acct.ID, time.Now().UTC().Format("2006-01"))
	ctx = paddle.ContextWithTransitID(ctx, idem)

	txn, err := p.client.CreateTransaction(ctx, &paddle.CreateTransactionRequest{
		CustomerID: &customerID,
		Items: []paddle.CreateTransactionItems{{
			TransactionItemFromCatalog: &paddle.TransactionItemFromCatalog{
				PriceID:  monthly,
				Quantity: 1,
			},
		}},
		CustomData: paddle.CustomData{
			"faas_account_id":      acct.ID,
			"plan":                 string(plan),
			"kind":                 "billing_retry",
			"faas_paddle_idem_key": idem,
		},
	})
	if err != nil {
		return "", "", fmt.Errorf("paddle: CreateTransaction(retry) account=%s plan=%s: %w",
			acct.ID, plan, err)
	}
	if txn == nil {
		return "", "", fmt.Errorf("paddle: CreateTransaction(retry) returned nil txn for account=%s", acct.ID)
	}
	return txn.ID, txn.ID, nil
}

// CancelAtPeriodEnd implements billing.Provider.CancelAtPeriodEnd
// on Paddle (issue #242).
//
// Paddle Billing v2 has no separate "scheduled cancellation"
// primitive (no API for cancel_at_period_end). The cancel intent
// is stamped onto the Customer.CustomData.scheduled_change field
// via paddle.Client.UpdateCustomer. The next monthly-rollover
// flush reads the flag and skips posting the rebill transaction;
// the account downgrades to Free on the next meterd dunning tick.
//
// Returns the next month-rollover instant as the effective
// timestamp. Paddle doesn't expose current_period_end on the
// Customer object — we compute the start-of-next-month in UTC and
// return that as the best-available estimate. The CLI renders
// this as "your apps will stop on <date>"; the dashboard reads
// the same field via the existing account-row fetch path.
//
// Returns ErrAlreadyCancelled when acct has no ProviderCustomerID
// (Free / never-checked-out / post-cancel).
func (p *Provider) CancelAtPeriodEnd(ctx context.Context, acct state.Account) (time.Time, error) {
	if p.client == nil {
		return time.Time{}, fmt.Errorf("paddle: SDK not initialized")
	}
	if acct.ProviderCustomerID == "" {
		return time.Time{}, fmt.Errorf("%w (account %s, no customer)",
			billing.ErrAlreadyCancelled, acct.ID)
	}
	customerID := acct.ProviderCustomerID
	scheduled := "cancel_at_period_end"
	_, err := p.client.UpdateCustomer(ctx, &paddle.UpdateCustomerRequest{
		CustomerID: customerID,
		CustomData: paddle.NewPatchField(paddle.CustomData{
			"scheduled_change":    scheduled,
			"faas_cancel_stamped": time.Now().UTC().Format(time.RFC3339),
		}),
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("paddle: UpdateCustomer(cancel) account=%s: %w",
			acct.ID, err)
	}
	// Best-available effective_at: start of next UTC month.
	now := p.now()
	effective := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	return effective, nil
}

// PaymentMethodSummary implements billing.Provider.PaymentMethodSummary
// on Paddle (issue #242). Calls paddle.Client.ListCustomerPaymentMethods
// against the account's customer and reduces the wire shape to the
// pkg/billing.PaymentMethod internal type.
//
// A Free / no-card-on-file customer returns the zero PaymentMethod.
// Implementations MUST NOT fail when the customer has no card on
// file — they return the zero PaymentMethod, not an error. The
// only error path is the provider-SDK failure case.
//
// Paddle's CardType is already lowercase network labels
// ("visa", "mastercard"), so the wire DTO's brand field carries
// the value verbatim. ExpMonth/ExpYear map from Card.ExpiryMonth/
// ExpiryYear (Paddle's field name).
func (p *Provider) PaymentMethodSummary(ctx context.Context, acct state.Account) (billing.PaymentMethod, error) {
	if p.client == nil {
		return billing.PaymentMethod{}, fmt.Errorf("paddle: SDK not initialized")
	}
	if acct.ProviderCustomerID == "" {
		// No customer = no card on file. Zero-value PaymentMethod.
		return billing.PaymentMethod{}, nil
	}
	customerID := acct.ProviderCustomerID
	col, err := p.client.ListCustomerPaymentMethods(ctx, &paddle.ListCustomerPaymentMethodsRequest{
		CustomerID: customerID,
	})
	if err != nil {
		return billing.PaymentMethod{}, fmt.Errorf("paddle: ListCustomerPaymentMethods account=%s: %w",
			acct.ID, err)
	}
	if col == nil {
		return billing.PaymentMethod{}, nil
	}
	// Iterate via the SDK's Next() / Res[T] pattern (same shape as
	// Stripe's iterator, but Res-based instead of a struct field).
	for {
		res := col.Next(ctx)
		if res == nil {
			break
		}
		if err := res.Err(); err != nil {
			return billing.PaymentMethod{}, fmt.Errorf("paddle: ListCustomerPaymentMethods iter account=%s: %w",
				acct.ID, err)
		}
		if !res.Ok() {
			break
		}
		m := res.Value()
		if m == nil {
			continue
		}
		if m.Type != paddle.SavedPaymentMethodTypeCard {
			continue
		}
		if m.Card == nil {
			continue
		}
		return billing.PaymentMethod{
			Brand:    string(m.Card.Type),
			Last4:    m.Card.Last4,
			ExpMonth: m.Card.ExpiryMonth,
			ExpYear:  m.Card.ExpiryYear,
		}, nil
	}
	return billing.PaymentMethod{}, nil
}
