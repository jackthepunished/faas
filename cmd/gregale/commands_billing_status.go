// commands_billing_status.go — `faas billing status` (PR-P3).
//
// Prints the active billing Provider name + the cached catalog
// snapshot. Backs the operator's at-a-glance "is Paddle wired up
// correctly?" check. The endpoint is admin-scoped + email-allowlist
// gated server-side; this CLI just renders the response.
//
// On a Stripe deployment the handler returns 501 with code
// billing_op_unsupported — the CLI surfaces that as a typed error
// so the operator knows the surface is Paddle-scoped, not a
// transport failure.

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
)

const billingSubStatus = "status"

// cmdBillingStatus renders the operator-facing billing status. The
// flow is intentionally minimal: one HTTP GET, one print. No flags
// today; future PR-P4 (runbook) may add --json for machine-readable
// output once operators ask for it.
func cmdBillingStatus(args []string) int {
	if len(args) != 0 {
		fmt.Fprintf(os.Stderr, "faas billing status: unexpected args\n")
		return 1
	}
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	resp, err := client.ListPaddleCatalog(context.Background())
	if err != nil {
		// The 501 path renders a hint specific to "this is a
		// provider-scoped surface, not a transport failure".
		// Branching on the problem code keeps the UX targeted.
		return printErr("Could not read billing catalog", err)
	}
	printBillingStatus(osStdout, resp)
	return 0
}

// printBillingStatus renders the catalog as a tab-aligned table.
// The first column is "plan / kind" so the operator can scan a row
// per (plan, kind) pair; the second column is the Paddle-side
// handle (pri_… / pro_…); the third is the SyncedAt timestamp.
//
// An empty catalog (no hydration yet) renders the "Provider: paddle,
// SyncedAt: never synced" header followed by a one-line hint to run
// `faas billing price-catalog sync`. We do not gate that hint on
// the response — the operator's CLI subcommand is the right place
// for actionable guidance.
func printBillingStatus(w io.Writer, resp api.BillingCatalogResponse) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	defer func() { _ = tw.Flush() }()

	_, _ = fmt.Fprintf(tw, "Provider:\t%s\n", resp.Provider)
	syncedAt := "never synced"
	if resp.SyncedAt != "" {
		syncedAt = resp.SyncedAt
	}
	_, _ = fmt.Fprintf(tw, "SyncedAt:\t%s\n", syncedAt)
	if len(resp.Entries) == 0 {
		_, _ = fmt.Fprintln(tw, "\nCatalog:\t<empty>")
		_, _ = fmt.Fprintln(tw, "\nRun `faas billing price-catalog sync` to hydrate.")
		return
	}
	_, _ = fmt.Fprintln(tw, "\nCatalog:")
	_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\n", "PLAN/KIND", "HANDLE", "SYNCED AT")
	for _, e := range resp.Entries {
		synced := "—"
		if !e.SyncedAt.IsZero() && !e.SyncedAt.Equal(time.Time{}) {
			synced = e.SyncedAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		_, _ = fmt.Fprintf(tw, "  %s/%s\t%s\t%s\n", e.Plan, e.Kind, e.Handle, synced)
	}
}
