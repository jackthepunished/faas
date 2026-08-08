// commands_billing_pricecatalog.go — `faas billing price-catalog` (PR-P3).
//
// Three sub-subcommands backed by the operator-facing admin endpoints:
//
//	list    GET    /v1/admin/billing-paddle-catalog
//	sync    POST   /v1/admin/billing-paddle-catalog/sync
//	reset   DELETE /v1/admin/billing-paddle-catalog
//
// list + sync share the same printer (printBillingStatus renders
// either response identically — same shape). reset prints a
// targeted warning because the handler is a no-op for Paddle
// (catalog is durable on the platform; deleting the in-memory
// cache would not unlink the merchant's prices).

package main

import (
	"context"
	"fmt"
	"io"
	"os"
)

const (
	billingSubPriceCatalog      = "price-catalog"
	billingSubPriceCatalogList  = "list"
	billingSubPriceCatalogSync  = "sync"
	billingSubPriceCatalogReset = "reset"
	priceCatalogResetWarning    = "Paddle's catalog is durable on the platform — resetting the in-memory cache does NOT delete the products.\n" +
		"Delete products via the Paddle Dashboard, then run `faas billing price-catalog sync` to re-hydrate."
)

// cmdBillingPriceCatalog dispatches `faas billing price-catalog <list|sync|reset>`.
// Bare subcommand prints usage to stderr and exits 1 (matches
// cmdBilling's bare-subcommand behaviour). Unknown sub-subcommand
// prints usage + the error to stderr.
func cmdBillingPriceCatalog(args []string) int {
	if len(args) == 0 {
		printPriceCatalogUsage(os.Stderr)
		return 1
	}
	switch args[0] {
	case billingSubPriceCatalogList:
		return cmdBillingPriceCatalogList(args[1:])
	case billingSubPriceCatalogSync:
		return cmdBillingPriceCatalogSync(args[1:])
	case billingSubPriceCatalogReset:
		return cmdBillingPriceCatalogReset(args[1:])
	case billingSubHelp, flagHelpShort, flagHelpLong:
		printPriceCatalogUsage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "faas billing price-catalog: unknown subcommand %q\n\n", args[0])
		printPriceCatalogUsage(os.Stderr)
		return 1
	}
}

// cmdBillingPriceCatalogList renders the cached catalog. Same shape
// as `faas billing status` — kept as a separate subcommand because
// operators think of "list" and "status" as different verbs
// (list = raw data; status = at-a-glance summary that includes the
// synced-at header). Two surfaces, one printer.
func cmdBillingPriceCatalogList(args []string) int {
	if len(args) != 0 {
		fmt.Fprintf(os.Stderr, "faas billing price-catalog list: unexpected args\n")
		return 1
	}
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	resp, err := client.ListPaddleCatalog(context.Background())
	if err != nil {
		return printErr("Could not read billing catalog", err)
	}
	printBillingStatus(osStdout, resp)
	return 0
}

// cmdBillingPriceCatalogSync forces an EnsurePlanProducts round-trip.
// The endpoint is idempotent on Paddle-side products so calling it
// twice in a row is safe (the second call walks only the LIST
// endpoints). The Idempotency-Key header is auto-UUIDv4 by the
// client; a flaky-network retry within 24h replays the original 200.
func cmdBillingPriceCatalogSync(args []string) int {
	if len(args) != 0 {
		fmt.Fprintf(os.Stderr, "faas billing price-catalog sync: unexpected args\n")
		return 1
	}
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	resp, err := client.SyncPaddleCatalog(context.Background(), "")
	if err != nil {
		return printErr("Paddle catalog sync failed", err)
	}
	_, _ = fmt.Fprintln(os.Stdout, "Synced. Catalog snapshot:")
	printBillingStatus(os.Stdout, resp)
	return 0
}

// cmdBillingPriceCatalogReset signals a catalog reset. The Paddle
// handler is a no-op; the CLI prints the warning so the operator
// knows what actually happens. Future PRs that add merchant-side
// cleanup will replace the warning with a success message.
func cmdBillingPriceCatalogReset(args []string) int {
	if len(args) != 0 {
		_, _ = fmt.Fprintf(os.Stderr, "faas billing price-catalog reset: unexpected args\n")
		return 1
	}
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	if _, err := client.ResetPaddleCatalog(context.Background()); err != nil {
		return printErr("Paddle catalog reset failed", err)
	}
	_, _ = fmt.Fprintln(os.Stdout, "Reset signal recorded.")
	_, _ = fmt.Fprintln(os.Stdout)
	_, _ = fmt.Fprintln(os.Stdout, priceCatalogResetWarning)
	return 0
}

// printPriceCatalogUsage prints the price-catalog dispatch help.
// Reuses the subcommand names from the const block above so a
// future rename trips one tripwire.
func printPriceCatalogUsage(w io.Writer) {
	_, _ = fmt.Fprintf(w, "usage: faas billing price-catalog <subcommand>\n\n"+
		"  %s    read the cached Paddle price + product catalog\n"+
		"  %s    force a Paddle catalog hydration (idempotent)\n"+
		"  %s   signal a catalog reset (Paddle: no-op; see warning)\n"+
		"\n"+
		"Run 'faas billing price-catalog help' for this message.\n",
		billingSubPriceCatalogList, billingSubPriceCatalogSync, billingSubPriceCatalogReset)
}
