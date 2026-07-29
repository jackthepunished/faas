// commands_audit_events.go — `faas audit-events` operator/customer CLI
// surface (Wave 0 PR-C / ADR-047). Wraps GET /v1/audit-events.
//
// Default shape is the customer-friendly one: lists the caller's
// own audit events, newest-first, capped at 50 (server-side bound;
// silently capped at 100). --kind-prefix filters (e.g.
// "stateless.advisory" for the runtime persistence advisory rows).
// --app-id filters to one app's events (the dashboard's per-app
// drill-down). --include-anonymous flips the include_anonymous
// query param so an operator can see subject=NULL rows (the
// defensive case where the app row was deleted between wake and the
// advisory emit).
//
// Auth: the route is s.auth + requireScope(api.ScopesReadSurface) —
// the same gating as the rest of the read surface. A session-cookie
// principal (Key == nil) implicitly carries admin scope per
// principalHasScope, so a dashboard customer can read their own log
// without holding an API key.

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"
)

// cmdAuditEvents implements `faas audit-events [--kind-prefix P]
// [--app-id <uuid>] [--since RFC3339] [--limit N]
// [--include-anonymous]`. Returns 0 on success, 2 on operator error
// (bad flags), 1 on transport / 5xx.
func cmdAuditEvents(args []string) int {
	fs := flag.NewFlagSet("audit-events", flag.ContinueOnError)
	kindPrefix := fs.String("kind-prefix", "", "filter by `kind` prefix (e.g. stateless.advisory)")
	appID := fs.String("app-id", "", "filter to one app's events (matches data.app_id)")
	since := fs.String("since", "", "RFC 3339 lower bound on `at`")
	limit := fs.Int("limit", 50, "max rows (1..100; server caps at 100)")
	includeAnon := fs.Bool("include-anonymous", false, "also surface subject=NULL rows (operator post-mortem)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: faas audit-events [--kind-prefix P] [--app-id <uuid>] [--since RFC3339] [--limit N] [--include-anonymous]")
		return 2
	}
	if *since != "" {
		if _, err := time.Parse(time.RFC3339, *since); err != nil {
			fmt.Fprintf(os.Stderr, "faas: --since must be RFC 3339 (e.g. 2026-07-25T00:00:00Z): %v\n", err)
			return 2
		}
	}

	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	resp, err := client.ListAuditEvents(ctx, *since, *kindPrefix, *appID, *limit, *includeAnon)
	if err != nil {
		return printErr("Could not list audit events", err)
	}
	for _, e := range resp.Events {
		// Subject may be empty when --include-anonymous surfaced a
		// subject=NULL row; print "-" so the column stays aligned.
		subject := e.Subject
		if subject == "" {
			subject = "-"
		}
		fmt.Printf("%s\t%s\t%s\t%s\n", e.At, e.Actor, e.Kind, subject)
	}
	return 0
}
