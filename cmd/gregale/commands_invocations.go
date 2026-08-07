// commands_invocations.go — `gregale invocations <list|get>` (Tier C).
// Closes the audit gap for the /v1/invocations surface (issue #394
// follow-up: the per-account invocation ledger was SDK-only). The
// dashboard renders the same list under the invocation-log panel;
// this leaf is the scriptable twin.
//
// Auth: self (route is auth + requireMFA + ScopesReadSurface).
//
// Back-compat forwarder (Tier B pattern, mirrors cmdAuditEvents):
// `gregale invocations --before X` forwards to `invocations list`,
// keeping pre-PR-#722 scripts alive.

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/onebox-faas/faas/pkg/api"
)

func cmdInvocations(args []string) int {
	if len(args) == 0 {
		PrintUsage(os.Stderr, "usage: gregale invocations <list|get <id>>", "invocations")
		return 1
	}
	if strings.HasPrefix(args[0], "-") {
		return cmdInvocationsList(args)
	}
	switch args[0] {
	case subList:
		return cmdInvocationsList(args[1:])
	case "get":
		return cmdInvocationsGet(args[1:])
	}
	fmt.Fprintf(os.Stderr, "unknown invocations subcommand %q\n", args[0])
	return 1
}

// cmdInvocationsList implements `gregale invocations list
// [--before <cursor>] [--limit N]`. Newest first. Auth surface same
// as cmdAuditEventsList but the server emits invocation rows, not
// audit-event rows, so the renderer shape is different.
func cmdInvocationsList(args []string) int {
	fs := flag.NewFlagSet("invocations list", flag.ContinueOnError)
	before := fs.String("before", "", "pagination cursor (NextBefore from a prior call)")
	limit := fs.Int("limit", 50, "max rows (1..100; server caps at 100)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: gregale invocations list [--before C] [--limit N]")
		return 1
	}
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	resp, err := client.ListInvocations(context.Background(), *before, *limit)
	if err != nil {
		return printErr("Could not list invocations", err)
	}
	if jsonOutput {
		return jsonOut(writeJSON(resp))
	}
	if len(resp.Invocations) == 0 {
		_, _ = fmt.Fprintln(osStdout, "(no invocations)")
		return 0
	}
	for _, inv := range resp.Invocations {
		fmt.Printf("%s\t%s\t%s\t%s\t%s\n", inv.ID, inv.CreatedAt.Format("2006-01-02T15:04:05Z07:00"), inv.State, inv.Method, inv.Path)
	}
	return 0
}

// cmdInvocationsGet fetches one invocation by id. Same posture as
// cmdAuditEventsGet: the list is newest-first capped at 100, so a
// deeply old row is unreachable via scrolling.
func cmdInvocationsGet(args []string) int {
	fs := flag.NewFlagSet("invocations get", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 1 {
		PrintUsage(os.Stderr, "usage: gregale invocations get <id>", "invocations")
		return 1
	}
	id := fs.Arg(0)
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	resp, err := client.GetInvocation(context.Background(), id)
	if err != nil {
		return printErr("Could not fetch invocation", err)
	}
	if jsonOutput {
		return jsonOut(writeJSON(resp))
	}
	fmt.Printf("id:           %s\n", resp.ID)
	fmt.Printf("state:        %s\n", resp.State)
	fmt.Printf("source:       %s\n", resp.Source)
	fmt.Printf("method:       %s\n", resp.Method)
	fmt.Printf("path:         %s\n", resp.Path)
	fmt.Printf("app_id:       %s\n", resp.AppID)
	fmt.Printf("instance_id:  %s\n", resp.InstanceID)
	fmt.Printf("due_at:       %s\n", resp.DueAt.Format("2006-01-02T15:04:05Z07:00"))
	if resp.CompletedAt != nil {
		fmt.Printf("completed_at: %s\n", resp.CompletedAt.Format("2006-01-02T15:04:05Z07:00"))
	}
	fmt.Printf("attempts:     %d\n", resp.Attempts)
	if resp.LastError != "" {
		fmt.Printf("last_error:   %s\n", resp.LastError)
	}
	return 0
}

// Sanity reference: keep Invocation in the import graph for the
// --json output path. The renderer reads no fields directly here
// (cmdAuditEventsList uses jsonOut + writeJSON) but the compiler
// will drop the import if the leaf body shrinks in the future.
var _ = api.Invocation{}
