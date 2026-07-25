package paddle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/PaddleHQ/paddle-go-sdk/v5"
	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// Sentinel errors for the pre-SDK guard failures. The classifier in
// errors.go uses errors.Is to map these to stable Prometheus labels
// instead of string-fragment matching — adding a sentinel is the
// supported way to introduce a new pre-SDK failure mode.
//
// Mirrors pkg/billing/stripe/usage.go:17-30 (the Stripe pair):
//
//   - ErrNoAPIKey / stripe.ErrNoAPIKey
//   - ErrNegativeMBSeconds / stripe.ErrNegativeQuantity
//     (Paddle wire quantity is 1; the guard is on the int64
//     mb_seconds input, not on wire quantity, so the name differs)
//   - ErrOveragePriceMissing — Paddle-specific; no Stripe analog
//     because Stripe's metered subscription_item is provisioned
//     once and reused, while Paddle's overage price handle is
//     looked up per push from the boot-time catalog. If
//     EnsurePlanProducts has not populated the catalog, the
//     push fails fast with this sentinel before the SDK is
//     invoked.
var (
	ErrNoAPIKey            = errors.New("paddle: cannot push usage without apiKey")
	ErrNegativeMBSeconds   = errors.New("paddle: negative mb_seconds")
	ErrOveragePriceMissing = errors.New("paddle: overage price missing for plan")
)

// flushOverageLocked is the cross-process dedupe gate + SDK post
// for one (account, month) push. Stateless: no in-memory
// accumulator. Each call's `hour` argument is the meterd tick's
// timestamp; the calendar month is derived via calendarMonthStart.
//
// PR #179's Has/Record gate stays as the durable idempotency
// surface. The within-process "flushed" stamp that the old
// pendingOverage map provided is now provided by the state.Store
// row itself — the meterd loop is single-goroutine, so concurrent
// pushes for the same (account, month) cannot happen, but a
// redelivered month across process boots is still a no-op.
//
// Defensive zero-sum guard lives here too — flushOverageLocked is
// reachable from PushUsageRecord after the pre-SDK guards (which
// already short-circuit on 0) and from any future caller that
// wants to bypass the guards. Idempotent no-op on 0.
func (p *Provider) flushOverageLocked(ctx context.Context, acct state.Account, hour time.Time, mbSeconds int64) error {
	if mbSeconds == 0 {
		return nil
	}
	monthStart := calendarMonthStart(hour.UTC())
	if p.dedupe != nil {
		already, err := p.dedupe.HasPaddleOverageMonth(ctx, acct.ID, monthStart)
		if err != nil {
			return fmt.Errorf("paddle: dedupe has month=%s acct=%s: %w",
				monthStart.Format("2006-01"), acct.ID, err)
		}
		if already {
			// Prior process boot (or a prior tick within this boot)
			// already flushed this month. Skip the SDK POST.
			return nil
		}
	}
	flusher := p.flushFn
	if flusher == nil {
		flusher = defaultFlushLocked
	}
	if err := flusher(ctx, p, acct, monthStart, mbSeconds); err != nil {
		return err
	}
	if p.dedupe != nil {
		if err := p.dedupe.RecordPaddleOverageMonth(ctx, acct.ID, monthStart); err != nil {
			return fmt.Errorf("paddle: dedupe record month=%s acct=%s: %w",
				monthStart.Format("2006-01"), acct.ID, err)
		}
	}
	return nil
}

// calendarMonthStart returns the first instant of t's UTC calendar
// month. Pulled out so the math is testable without driving the
// dedupe gate. Reference values: Feb 1, Mar 1, the leap-day edge
// (Feb 29 in leap years), and the Dec → Jan year boundary.
//
// (Reviewer ask from PR #179: the previous Truncate(30*24h).
// Truncate(time.Hour) only accidentally produced a month boundary
// on 30-day months — February pushed Feb 28 23:59 would bucket into
// the Jan 30 line, which then never flushed against Feb's actual
// month. The replaced function below is the correct one.)
func calendarMonthStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// FlushFn is the seam flushOverageLocked delegates to. Each call
// builds the actual `CreateTransaction` SDK request and returns an
// error; a test stub can substitute a counter to drive cross-month
// flush semantics in unit tests without the SDK.
//
// Keep this signature stable — tests reach for it directly via
// provider.flushFn.
type FlushFn func(ctx context.Context, p *Provider, acct state.Account, monthStart time.Time, mbSeconds int64) error

// defaultFlushLocked is the production FlushFn: looks up the
// overage price handle for the account's plan, posts a quantity-1
// Transactions line item with the (acct, month) Idempotency-Key
// stamped into CustomData AND forwarded as a transport-level
// Idempotency-Key HTTP header via the wrapper at transport.go.
//
// The Idempotency-Key injection happens via the SDK's
// ContextWithTransitID (which the SDK stamps as X-Transit-Id on
// the outbound request); our RoundTripper at transport.go reads
// X-Transit-Id and copies it as Idempotency-Key on POST /transactions.
// Paddle's API server may not honor the header today (SDK team is
// working on native support); the header is observable on the
// wire for ops debugging and is forward-compat.
func defaultFlushLocked(ctx context.Context, p *Provider, acct state.Account, monthStart time.Time, mbSeconds int64) error {
	priceID := p.overagePriceForPlan(acct.Plan)
	if priceID == "" {
		return fmt.Errorf("%w (plan=%s)", ErrOveragePriceMissing, acct.Plan)
	}
	idem := fmt.Sprintf("faas-overage-%s-%s", acct.ID, monthStart.Format("2006-01"))
	customerID := acct.ProviderCustomerID // column name stale per ADR-025; rename is a follow-up migration PR

	// Stamp the transit ID on the context. The SDK's internal/client
	// (client.go:98-101) reads this and sets X-Transit-Id on the
	// outbound request; our transport wrapper copies it as
	// Idempotency-Key on POST /transactions. Single source of truth
	// for the idempotency value across the SDK header + our injected
	// header + the CustomData field.
	ctx = paddle.ContextWithTransitID(ctx, idem)

	_, err := p.client.CreateTransaction(ctx, &paddle.CreateTransactionRequest{
		CustomerID: &customerID,
		Items: []paddle.CreateTransactionItems{{
			TransactionItemFromCatalog: &paddle.TransactionItemFromCatalog{
				PriceID:  priceID,
				Quantity: 1,
			},
		}},
		CustomData: paddle.CustomData{
			"faas_account_id":      acct.ID,
			"month":                monthStart.Format("2006-01"),
			"mb_seconds":           fmt.Sprintf("%d", mbSeconds),
			"faas_paddle_idem_key": idem,
		},
	})
	if err != nil {
		return fmt.Errorf("paddle: CreateTransaction: %w", err)
	}
	return nil
}

// overagePriceForPlan returns the overage line-item price handle for
// a plan, from the priceCatalog.
func (p *Provider) overagePriceForPlan(plan api.Plan) string {
	p.catalog.mu.RLock()
	defer p.catalog.mu.RUnlock()
	return p.catalog.planOverage[plan]
}
