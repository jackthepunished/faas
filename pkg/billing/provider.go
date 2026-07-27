// Package billing is the per-deployment abstraction over external payment
// processors (ADR-025). One Provider is selected at boot from
// FAAS_BILLING_PROVIDER; the selected implementation is the only registered
// call site for whatever vendor SDK we use, so the surface stays
// audit-friendly.
//
// The interface is intentionally narrow — the four primitives M7 needs —
// so adding a third provider is a one-package PR. Concrete
// implementations today:
//
//   - pkg/billing/stripe — extracted from the original pkg/stripex package.
//     Default provider when FAAS_BILLING_PROVIDER is empty.
//   - pkg/billing/paddle — Paddle Billing v2 (current API). Opt-in via
//     FAAS_BILLING_PROVIDER=paddle.
//
// Provider-specific behaviour stays inside each implementation; the rest
// of the codebase (apid, meterd, the dunning state machine, the email
// surface) talks only to the Provider interface.
package billing

import (
	"context"
	"errors"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// Provider is the per-deployment abstraction apid and meterd use. The
// selected implementation handles every external billing call (product
// setup, customer creation, hourly usage push, webhook verification).
// apid's webhook handler dispatches to the same Provider after a
// successful VerifyWebhook call.
//
// Implementations MUST be safe for concurrent use; meterd's quota + dunning
// loops and apid's webhook ingress call into Provider from multiple
// goroutines.
type Provider interface {
	// EnsurePlanProducts is the idempotent product/price setup at boot.
	// Stripe: stripe.Plans.List + stripe.Plans.New by Nickname. Paddle:
	// paddle.Items.List + paddle.Items.Create by description match.
	// Idempotent across restarts so a redelivered boot is a no-op.
	EnsurePlanProducts(ctx context.Context) error

	// CreateCustomer maps a state.Account to the provider's customer
	// handle and writes the ID back onto the account row. The column
	// the ID lands in is named after the Stripe-only era today; a
	// column rename is out of scope for ADR-025 (separate, smaller
	// migration PR).
	CreateCustomer(ctx context.Context, acct state.Account) (string, error)

	// PushUsageRecord is the meterd pusher. Stripe: per-hour metered
	// UsageRecord against the customer's subscription item. Paddle:
	// at month-rollover, posts a flat-rate line item for the prior
	// month's accumulated mb_seconds; non-rollover calls accumulate
	// internally.
	//
	// Signature is symmetric so meterd's loop is implementation-agnostic.
	// The dedupe contract (a redelivered hour is a no-op) is the
	// implementation's responsibility — implementations should
	// idempotency-key every external call against (acct.ID, hour-or-month).
	PushUsageRecord(ctx context.Context, acct state.Account, hour time.Time, mbSeconds int64) error

	// VerifyWebhook checks a provider-shaped signature header against
	// the body and returns a normalized Event. apid then matches the
	// Event against the dunning state machine — apid never sees
	// provider-shaped JSON.
	//
	// tolerance caps the timestamp window (Stripe: Stripe-Signature `t=`;
	// Paddle: Paddle-Signature `ts=`). Empty header / bad signature
	// returns ErrBadSignature, wrapped with operation context.
	VerifyWebhook(payload []byte, headers map[string]string, tolerance time.Duration) (Event, error)

	// CreateUpgradeTransaction materializes the provider's hosted-checkout
	// surface for an upgrade to targetPlan. apid's changePlan handler
	// calls this when an account's plan is being upgraded and the
	// customer has no active subscription item yet — the typical
	// free → paid direct path (spec §4.7).
	//
	//   - Paddle: calls paddle.Client.CreateTransaction against the
	//     per-plan monthly price and returns (txn_…,
	//     https://paddle.checkout/…, nil). The 402 Problem carries
	//     these as PaddleCheckoutURL + TxID extensions so the dashboard
	//     can render an upsell button + confirmation id.
	//   - Stripe: returns ("", "", nil) — a deliberate signal that the
	//     apid handler should fall back to the precomputed
	//     FAAS_BILLING_PORTAL_URL template (with {account_id}
	//     substituted). Stripe's billing-portal session is operator-
	//     configured, not SDK-created.
	//
	// The "(txID == \"\") ⇒ Stripe stub" contract is the dispatch signal
	// the apid handler branches on. Implementations should add a stable
	// Idempotency-Key (Paddle: recorded in CustomData) so a redelivered
	// upgrade click doesn't create a duplicate Transaction.
	CreateUpgradeTransaction(ctx context.Context, acct state.Account, targetPlan api.Plan) (txID, checkoutURL string, err error)

	// Refund issues an operator-initiated refund against a charge
	// (issue #279). amountCents is integer cents (the financial
	// model's unit; never float on money). The implementation is
	// responsible for:
	//
	//   1. Translating cents → the provider's native unit (Stripe:
	//      millicents; Paddle: not implemented → returns
	//      ErrNotImplemented).
	//   2. Forwarding a ctx-derived Idempotency-Key so a network-blip
	//      retry does not create a duplicate refund. Stripe: read
	//      via idempotencyKeyFromCtx; Paddle: not implemented.
	//   3. Mapping provider errors (Stripe: amount_too_large,
	//      charge_already_refunded, etc.) onto errors the apid
	//      handler can dispatch on. Today the handler returns
	//      502 for any non-nil error; the refinement is a follow-up.
	//
	// apid's handler is the only caller (cmd/apid/handlers_admin_credits.go).
	// The webhook (charge.refunded) is observational and routes
	// through VerifyWebhook → EventRefundProcessed, NOT through this
	// method.
	Refund(ctx context.Context, chargeID string, amountCents int64) (*RefundResult, error)
}

// EventType is the provider-neutral "what happened" classifier apid
// dispatches on. Mapping from the provider's payload lives inside each
// implementation's VerifyWebhook.
type EventType int

const (
	// EventUnknown is the zero value; VerifyWebhook returns it when the
	// provider-specific event has no mapping (the apid handler treats
	// it as a no-op 200 — Stripe expects 2xx for everything it didn't
	// recognize so it doesn't retry forever).
	EventUnknown EventType = iota

	// EventSubscriptionCreated is fired when a customer completes
	// first-time checkout. apid uses it to stamp the customer's
	// stripe_subscription_item on the account row.
	EventSubscriptionCreated

	// EventSubscriptionUpdated is fired on plan changes mid-cycle.
	// apid syncs accounts.plan from the provider's payload.
	EventSubscriptionUpdated

	// EventSubscriptionCanceled is fired when the customer or the
	// provider cancels the subscription. apid flips the account to
	// suspended.
	EventSubscriptionCanceled

	// EventSubscriptionPastDue is fired when the provider marks the
	// subscription past-due (mid-cycle failure, after grace). apid
	// flips the account to past_due.
	EventSubscriptionPastDue

	// EventPaymentSucceeded is fired when an invoice settles. On a
	// past_due → active flip, apid sends the recovery email.
	EventPaymentSucceeded

	// EventPaymentFailed is fired when a charge bounces. apid flips
	// the account active → past_due and sends the entry-point email.
	EventPaymentFailed

	// EventRefundProcessed is fired when a refund is issued against a
	// charge (Stripe: charge.refunded). apid emits a `refund.processed`
	// audit event with the operator's account ID and the refund
	// amount. The webhook is observational — the operator-initiated
	// refund path goes through Provider.Refund (below), not through
	// this event.
	EventRefundProcessed
)

// Name returns the canonical English label apid's log lines + audit
// ledger use. The strings are stable — the cmd/apid events audit-log
// metric (events_audit_log_emission) and the dunning timer key off
// these names, not the integer values.
func (t EventType) Name() string {
	switch t {
	case EventSubscriptionCreated:
		return "subscription_created"
	case EventSubscriptionUpdated:
		return "subscription_updated"
	case EventSubscriptionCanceled:
		return "subscription_canceled"
	case EventSubscriptionPastDue:
		return "subscription_past_due"
	case EventPaymentSucceeded:
		return "payment_succeeded"
	case EventPaymentFailed:
		return "payment_failed"
	case EventRefundProcessed:
		return "refund_processed"
	default:
		return "unknown"
	}
}

// Event is the normalized envelope apid's dunning state machine
// dispatches on. Provider-shaped JSON stays inside each
// implementation; Raw carries the original body for debugging.
type Event struct {
	// Type drives the apid switch statement. Unknown / unmapped
	// types render as a 200 no-op.
	Type EventType

	// CustomerID is the provider's customer handle (Stripe: cus_…,
	// Paddle: ctm_…). apid resolves this to a state.Account via
	// Store.AccountByProviderCustomerID.
	CustomerID string

	// PlanID is the provider's plan identifier (Stripe: plan_… /
	// price_…, Paddle: pri_…). apid maps it to api.Plan via
	// PlanFromProviderID; empty when the event carries no plan
	// change (payment events typically don't).
	PlanID string

	// SubscriptionID is the provider's subscription handle (Stripe:
	// sub_…, Paddle: sub_…). apid may stamp this on the account
	// row if empty.
	SubscriptionID string

	// Raw is the original webhook body, preserved for the audit log
	// and for downstream debugging. Provider-shaped JSON.
	Raw []byte

	// AmountCents is the integer-cents amount carried by the event
	// (refund events: the refund amount; payment events: the
	// amount_paid; subscription events: 0). Providers populate
	// during VerifyWebhook so the apid handler can stamp the
	// audit-log payload without re-parsing Raw.
	AmountCents int64

	// Currency is the provider's three-letter currency code (Stripe:
	// string(r.Currency)). Empty when the event carries no monetary
	// value or the provider did not populate it.
	Currency string

	// ProviderRefundID is the provider's refund handle (Stripe: re_…).
	// Only populated for refund events. apid logs it on the
	// `refund.processed` audit row.
	ProviderRefundID string

	// ChargeID is the provider's charge handle (Stripe: ch_…; Paddle:
	// tx_…). Only populated for refund events. apid logs it so an
	// operator can correlate the audit row with the provider
	// dashboard.
	ChargeID string
}

// RefundResult is what Provider.Refund returns on a successful refund.
// The handler stamps the fields onto the audit row and echoes the
// ProviderRefundID to the CLI so an operator can pull up the refund
// in the provider dashboard by ID.
type RefundResult struct {
	ProviderRefundID string
	ChargeID         string
	AmountCents      int64
	Currency         string
}

// ErrBadSignature is the unified error returned by VerifyWebhook when
// the signature header is malformed, missing, the timestamp is out of
// tolerance, or the HMAC does not match. Provider implementations
// must wrap with %w so callers can use errors.Is.
var ErrBadSignature = errors.New("billing: bad webhook signature")

// ErrNotImplemented is the unified error a Provider returns when the
// selected billing backend does not support a method (issue #279:
// Paddle's Refund). Callers should map this to a 501 Problem with
// docs_url pointing at the spec — the operator picks a backend that
// supports the surface they need.
var ErrNotImplemented = errors.New("billing: provider does not implement this method")

// Classifier is the optional seam a Provider can implement to declare
// its push-error classification. meterd's pusher loop dispatches via
// this interface first so SDK-typed classification stays in the
// provider's own package (which knows about *stripe.Error /
// *paddleerr.Error) without forcing the billing.Provider interface
// wider. Returning "other" for an unknown inner error is the
// provider's contract; nil always returns "ok".
//
// Providers that don't implement this interface get meterd's default
// fallback ("other") — same as the prior all-Stripe dispatch. The
// pusher's opLabel/observer dispatch falls back to a Stripe-shaped
// histogram so a missing Classifier doesn't lose observations.
//
// Keep the label set closed per provider: pkg/billing/stripe's
// stripe.PushResultLabels and pkg/billing/paddle's paddle.PushResultLabels
// are each pre-instantiated in pkg/wire/metrics.go at registry
// construction time.
type Classifier interface {
	ClassifyPushError(err error) string
}
