// commands_billing_retry.go — `faas billing retry` (issue #242).
//
// Closes the customer-trust lie in the dunning email:
// `pkg/mail/account.go:107` (AccountSuspendedBody) and `:150`
// (PaymentFailedBody) promise the customer they can recover
// from a failed card by running `faas billing retry`; this
// subcommand is the implementation.
//
// Calls POST /v1/billing/retry. apid dispatches to the active
// billing Provider's RetryLatestCharge method — Stripe: Invoices.Pay
// on the latest open invoice; Paddle: CreateTransaction against
// the existing customer. Idempotency-Key is pinned server-side
// so a flaky-network redelivery collapses to one provider attempt.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
)

// cmdBillingRetry issues the retry. No body, no path params;
// the server resolves the account from the bearer token.
//
// Exit codes mirror the rest of the billing CLI:
//
//	0 — new attempt created
//	1 — user error (e.g. no open charge, the account is in good
//	    standing; the dunning email was stale)
//	3 — vendor failure (Stripe / Paddle SDK error)
func cmdBillingRetry(args []string) int {
	fs := flag.NewFlagSet("billing retry", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "usage: faas billing retry\n")
		return 1
	}

	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	resp, err := client.PostBillingRetry(context.Background())
	if err != nil {
		// Map Problem codes to friendly CLI hints. apid returns
		// 404 with code=billing_no_open_charge when the account
		// is in good standing; render the friendly hint instead
		// of the SDK error verbatim.
		var apiErr *api.APIError
		if errors.As(err, &apiErr) && apiErr.Problem.Code == "billing_no_open_charge" {
			return printErr("No open charge to retry — your account is in good standing", err)
		}
		return printErr("Retry failed", err)
	}

	if jsonOutput {
		return jsonOut(writeJSON(resp))
	}
	PrintOK(osStdout, "Retried charge.")
	_, _ = fmt.Fprintf(osStdout, "  attempt:   %s\n", resp.AttemptID)
	_, _ = fmt.Fprintf(osStdout, "  provider:  %s\n", resp.ProviderRefID)
	_, _ = fmt.Fprintf(osStdout, "  status:    %s\n", resp.Status)
	if resp.NextBillingAt != nil {
		_, _ = fmt.Fprintf(osStdout, "  next:      %s\n", resp.NextBillingAt.Format(time.RFC3339))
	}
	return 0
}
