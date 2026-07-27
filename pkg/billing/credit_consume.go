// Credit consumption reducer (issue #279 PR-C).
//
// The PR #337 / #279 PR-A surface only ISSUED credits; this package
// closes the loop by computing an invoice's overage in integer cents
// and draining the account's active credits FIFO against it.
//
// Today the only trigger is the operator endpoint
// POST /v1/invoices/{id}/consume-credits (cmd/apid/handlers_invoices_consume.go).
// Future callers — a meterd cron at month-rollover, the PR-B
// UpsertInvoice webhook Tx — call ConsumeCreditsForInvoice with their
// own actor string ("meterd", "apid-webhook"). The function never
// mutates the invoice row itself; that's PR-B's job. The reducer
// only writes to account_credits and credit_ledger.
//
// Money is integer cents end-to-end (CLAUDE.md). The overage math
// mirrors pgstore.CurrentMonthOverageCents:
//
//	mb_seconds * 100 / 3600   (1 GB-h = 100 cents at €0.01/GB-h)
//
// Floor division — under-collect at most 0.9 cents per invoice,
// matches the financial model's "credit the customer on rounding"
// convention. The function never round-trips through float.
package billing

import (
	"context"
	"errors"
	"fmt"

	"github.com/onebox-faas/faas/pkg/state"
)

// ComputeInvoiceOverageCents converts the account's usage_minutes for
// the invoice's billing period [PeriodStart, PeriodEnd) into integer
// cents of overage, floored. 1 GB-h = 100 cents at €0.01/GB-h.
//
// Spec §4.7 keeps the per-minute enforcement math in meterd; the
// reducer reads the same usage_minutes rows. The query is
// `SUM(mb_seconds)` over [PeriodStart, PeriodEnd), and the formula
// is the same `* 100 / 3600` integer-only conversion
// pgstore.CurrentMonthOverageCents uses for the monthly view — the
// reducer just narrows the window to the invoice's period.
//
// Returns 0 when the account had no usage in the period (Free /
// Hobby under quota). The caller treats 0 as a no-op target: the
// reducer consumes zero credits and emits zero audit rows.
func ComputeInvoiceOverageCents(ctx context.Context, store state.Store, inv state.Invoice) (int64, error) {
	if inv.PeriodEnd.Before(inv.PeriodStart) {
		return 0, fmt.Errorf("billing: invoice %s has PeriodEnd %v before PeriodStart %v", inv.ID, inv.PeriodEnd, inv.PeriodStart)
	}
	usages, err := store.UsageByAccount(ctx, inv.AccountID, inv.PeriodStart)
	if err != nil {
		return 0, fmt.Errorf("billing: invoice overage usage fetch: %w", err)
	}
	var mbSec int64
	for _, u := range usages {
		// usage_minutes.minute is per-minute; sum is the full
		// [PeriodStart, PeriodEnd) window's billable mb-seconds.
		// Per-plan subtraction of IncludedGBHours is intentionally
		// NOT done here — the Invoice's subtotal_cents column is
		// the post-included-quota number, but until PR-B's webhook
		// writer lands we have no provider-stamped subtotal. The
		// reducer reads the raw mb-seconds and treats any included
		// quota as the account's plan concern (the dashboard
		// surfaces overage-cents-applied-credits as a separate line
		// in the invoice JSON once PR-B ships).
		mbSec += u.MBSeconds
	}
	// mb_seconds * 100 / 3600 = cents. Integer math. Floor is
	// implicit in integer division in Go.
	cents := mbSec * 100 / 3600
	if cents < 0 {
		return 0, fmt.Errorf("billing: invoice overage went negative (mb_seconds=%d); rejecting", mbSec)
	}
	return cents, nil
}

// ConsumeCreditsForInvoice is the provider-neutral reducer. It looks
// up the invoice, computes overage cents from usage_minutes for the
// invoice's billing period, drains active credits FIFO, and returns
// the result.
//
// The store's ConsumeAccountCredit handles the per-credit UPDATE /
// INSERT and the dedupe on provider_invoice_id; the reducer is a thin
// orchestrator on top of that primitive. It returns the Invoice so the
// caller can stamp the audit row's provider_invoice_id and period_end
// fields without re-fetching.
//
// actor parameter lets the caller stamp the system identity on the
// consumption ledger row. Today: "apid" (the admin endpoint). Future
// callers stamp "meterd" (cron) or "apid-webhook" (PR-B Tx).
// reason parameter rides on each credit_ledger row's reason column
// for traceability; the per-credit audit row's reason is the operator
// text from POST /v1/admin/accounts/{id}/credits.
//
// Safe to call from: apid admin endpoint (today), meterd cron
// (future), PR-B webhook Tx (future). The function never mutates
// the invoice row.
func ConsumeCreditsForInvoice(ctx context.Context, store state.Store, invoiceID, actor, reason string) (state.ConsumeAccountCreditResult, state.Invoice, error) {
	if invoiceID == "" {
		return state.ConsumeAccountCreditResult{}, state.Invoice{}, errors.New("billing: ConsumeCreditsForInvoice: invoiceID required")
	}
	inv, err := store.GetInvoiceByID(ctx, invoiceID)
	if err != nil {
		return state.ConsumeAccountCreditResult{}, state.Invoice{}, fmt.Errorf("billing: invoice lookup: %w", err)
	}

	target, err := ComputeInvoiceOverageCents(ctx, store, inv)
	if err != nil {
		return state.ConsumeAccountCreditResult{}, state.Invoice{}, fmt.Errorf("billing: compute overage: %w", err)
	}

	res, err := store.ConsumeAccountCredit(ctx, state.ConsumeAccountCreditParams{
		AccountID:         inv.AccountID,
		TargetCents:       target,
		Provider:          inv.Provider,
		ProviderInvoiceID: inv.ProviderInvoiceID,
		InvoiceID:         inv.ID,
		Reason:            reason,
		Actor:             actor,
	})
	if err != nil {
		return state.ConsumeAccountCreditResult{}, inv, fmt.Errorf("billing: consume: %w", err)
	}
	return res, inv, nil
}
