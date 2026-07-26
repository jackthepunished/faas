// Issue #279 — operator CLI surface for billing operations.
//
// `faas admin credit <account_uuid> <cents> --reason <text>` is the
// operator's only path to refund/credit a customer without leaving
// the platform. The dispatch is single-level: `faas admin <sub>`,
// not `faas admin credit <account> <cents> ...`. Future subcommands
// (refund-via-stripe, set-overage-cap, etc.) land as additional
// dispatch arms in cmdAdmin.
//
// Auth model: the SDK call requires an admin-scoped API key
// (ScopesAdminOnly) AND the caller's email must be in
// FAAS_ADMIN_EMAILS. Both layers are enforced server-side; the CLI
// just surfaces a friendlier error if the call returns 403.

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strconv"

	"github.com/google/uuid"
)

// cmdAdmin is the dispatcher for `faas admin <subcommand>`. New
// subcommands go here. Empty args / unknown subcommands print
// usage and return 2 (the CLI convention for "operator error").
//
// Flag convention: flags precede positional args, mirroring Go's
// flag package (and the existing `faas account export --no-secrets`
// pattern at commands4.go:146). `--reason <text>` must come before
// the account uuid + cents positionals.
func cmdAdmin(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: faas admin <credit|refund> [args]")
		fmt.Fprintln(os.Stderr, "  faas admin credit --reason <text> <account_uuid> <cents>")
		return 2
	}
	switch args[0] {
	case "credit":
		return cmdAdminCredit(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "faas: unknown admin subcommand %q\n", args[0])
		return 2
	}
}

// cmdAdminCredit issues an account credit via POST /v1/admin/
// accounts/{id}/credits. The Idempotency-Key is a stable
// `cli-admin-credit-<hex>` so a flaky-network retry returns the same
// credit_id (mirrors cmdAccountDelete's `cli-delete-…` pattern).
//
// Account argument is the target's UUID, not the email. The server
// is the source of truth for account lookup; if the UUID is unknown
// the handler returns 404 with CodeNotFound. We validate the UUID
// shape client-side for a faster, friendlier 2.
func cmdAdminCredit(args []string) int {
	fs := flag.NewFlagSet("admin credit", flag.ContinueOnError)
	reason := fs.String("reason", "", "reason text (required, 3..500 chars)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: faas admin credit --reason <text> <account_uuid> <cents>")
		return 2
	}
	accountUUID, err := uuid.Parse(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "faas: account must be a UUID")
		return 2
	}
	cents, err := strconv.ParseInt(fs.Arg(1), 10, 64)
	if err != nil || cents <= 0 {
		fmt.Fprintln(os.Stderr, "faas: cents must be a positive integer (in EUR cents)")
		return 2
	}
	if *reason == "" {
		fmt.Fprintln(os.Stderr, "faas: --reason is required (3..500 chars)")
		return 2
	}
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	// Stable Idempotency-Key so a retry returns the same credit_id.
	// The server stores the response for 24 h keyed on (caller, key).
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	key := "cli-admin-credit-" + hex.EncodeToString(nonce)
	resp, err := client.IssueAccountCredit(context.Background(), accountUUID.String(), key, cents, *reason)
	if err != nil {
		return printErr("Issue failed", err)
	}
	if jsonOutput {
		return jsonOut(writeJSON(resp))
	}
	PrintOK(osStdout, "Issued credit %s for %d cents to %s", resp.ID, resp.CentsRemaining, resp.AccountID)
	_, _ = fmt.Fprintf(osStdout, "  reason:    %s\n", resp.Reason)
	_, _ = fmt.Fprintf(osStdout, "  created:   %s\n", resp.CreatedAt.Format("2006-01-02T15:04:05Z07:00"))
	return 0
}
