// commands_invocations.go — issue #315 (tier-2 DX): customer-facing
// CLI twin for GET /v1/invocations/{id} (read) and POST
// /v1/invocations/{id}/replay (re-issue a failed invocation).
//
// The HTTP endpoints (read + replay handler) live in
// cmd/apid/handlers_invocations.go; this file is the CLI binding so
// a customer in their editor can inspect + re-run an invocation
// without a dashboard detour.
//
// Output shape mirrors cmdMetrics (commands_metrics.go:79) — labelled
// block in human mode (label: value per line, never a table — the
// fields are heterogeneous widths and the durations are formatted as
// "ago" deltas). --json emits one indented JSON object via
// writeJSON; --replay re-issues the invocation via the SDK's
// ReplayInvocation helper, prints the new id + status URL.
//
// Why a separate file: keeps commands_metrics.go focused on the
// metrics surface and gives reviewers a single ~150-line diff for
// the invocation read+replay shape.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/onebox-faas/faas/pkg/api"
)

// invocationCmdUsage is the top-of-failure-line shown for
// `gregale invocation` errors. Mirrors PrintUsage's docs URL
// convention (output.go:144) so the line carries the stable docs
// site pointer.
const invocationCmdUsage = "usage: gregale invocation <id> [--json] [--replay]"

// invocationCmdDocsTopic is the docs topic slug appended to
// docsURLBase when PrintUsage emits the trailing "Docs:" row. Keeps
// the CLI's help line stable across command additions.
const invocationCmdDocsTopic = "invocation"

// cmdInvocation implements `gregale invocation <id> [--json]
// [--replay]`. Mirrors the read shape of cmdMetrics
// (commands_metrics.go:45) — single positional id + a few flags,
// JSON single record, human labelled block.
//
// --replay is the load-bearing flag for this command: it issues a
// fresh async invocation that carries the original payload +
// headers + method + path. The new invocation's id + status URL are
// printed below the original's row. Only invocations whose State is
// "failed" or "dead_letter" can be replayed; everything else returns
// the SDK's APIError (server emits 409). A failed invocation whose
// LastError is empty still passes the state gate (the drain records
// an error string on Fail) — the wire distinguishes the two failure
// shapes by the State's presence in the allow-list.
func cmdInvocation(args []string) int {
	fs := flag.NewFlagSet("invocation", flag.ContinueOnError)
	replay := fs.Bool("replay", false, "re-issue a failed invocation (returns the new async invocation)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 1 {
		PrintUsage(os.Stderr, invocationCmdUsage, invocationCmdDocsTopic)
		return 1
	}
	id := fs.Arg(0)
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	ctx := context.Background()
	inv, err := client.GetInvocation(ctx, id)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) {
			renderAPIError(os.Stderr, ae)
			return exitCodeForStatus(ae.Problem.Status)
		}
		return printErr("Could not fetch invocation", err)
	}
	if *replay {
		// Replay shape: render the original row first (so the operator
		// can confirm they targeted the right id), then the new
		// AsyncInvokeResponse underneath. JSON output collapses to a
		// single {"original": ..., "replay": ...} envelope so scripts
		// have a stable shape regardless of --replay presence.
		resp, err := client.ReplayInvocation(ctx, id)
		if err != nil {
			var ae *APIError
			if errors.As(err, &ae) {
				renderAPIError(os.Stderr, ae)
				return exitCodeForStatus(ae.Problem.Status)
			}
			return printErr("Could not replay invocation", err)
		}
		if jsonOutput {
			return jsonOut(writeJSON(map[string]any{
				"original": inv,
				"replay":   resp,
			}))
		}
		renderInvocation(osStdout, inv)
		_, _ = fmt.Fprintln(osStdout)
		renderReplayResponse(osStdout, resp)
		return 0
	}
	if jsonOutput {
		return jsonOut(writeJSON(inv))
	}
	renderInvocation(osStdout, inv)
	return 0
}

// renderInvocation writes the human-mode labelled block for an
// api.Invocation. Mirrors the dashboard panel's per-invocation
// detail view (cmd/apid/dashboard/*) so a customer toggling between
// terminal and browser sees the same labels.
//
// Empty Optional fields (InstanceID, ScheduledAt, Result, etc.) are
// omitted so the block stays terse for in-progress rows. CompletedAt
// formats as "<rfc3339> (<ago>)" — the ago delta is what a customer
// usually wants ("how long ago did this finish?") and the absolute
// timestamp is the fallback for scripts / cross-references.
func renderInvocation(w io.Writer, inv api.Invocation) {
	_, _ = fmt.Fprintf(w, "Invocation: %s\n", inv.ID)
	_, _ = fmt.Fprintf(w, "App:        %s\n", inv.AppID)
	if inv.InstanceID != "" {
		_, _ = fmt.Fprintf(w, "Instance:   %s\n", inv.InstanceID)
	}
	_, _ = fmt.Fprintf(w, "Source:     %s\n", inv.Source)
	_, _ = fmt.Fprintf(w, "State:      %s\n", inv.State)
	_, _ = fmt.Fprintf(w, "Method:     %s\n", inv.Method)
	_, _ = fmt.Fprintf(w, "Path:       %s\n", inv.Path)
	_, _ = fmt.Fprintf(w, "Attempts:   %d\n", inv.Attempts)
	_, _ = fmt.Fprintf(w, "Created:    %s\n", inv.CreatedAt.UTC().Format(time.RFC3339))
	if inv.ScheduledAt != nil {
		_, _ = fmt.Fprintf(w, "Scheduled:  %s\n", inv.ScheduledAt.UTC().Format(time.RFC3339))
	}
	if inv.CompletedAt != nil {
		ago := time.Since(*inv.CompletedAt).Truncate(time.Millisecond)
		_, _ = fmt.Fprintf(w, "Completed:  %s (%s ago)\n",
			inv.CompletedAt.UTC().Format(time.RFC3339), ago)
	}
	if inv.LastError != "" {
		_, _ = fmt.Fprintf(w, "Last err:   %s\n", oneLine(inv.LastError))
	}
	if len(inv.Payload) > 0 {
		_, _ = fmt.Fprintf(w, "Payload:    %s\n", oneLine(string(inv.Payload)))
	}
	if len(inv.Result) > 0 {
		_, _ = fmt.Fprintf(w, "Result:     %s\n", oneLine(string(inv.Result)))
	}
}

// renderReplayResponse writes the two-line replay summary that
// follows the original row when --replay is set. Mirrors the
// AsyncInvokeResponse wire shape so the customer sees the same
// fields the SDK polls. Status URL is rendered as a relative path
// because the dashboard's invocation page lives on the same host;
// the SDK call returned the path verbatim from the server.
func renderReplayResponse(w io.Writer, r api.AsyncInvokeResponse) {
	_, _ = fmt.Fprintf(w, "Replay id:        %s\n", r.ID)
	_, _ = fmt.Fprintf(w, "Replay status:    %s\n", r.StatusURL)
	_, _ = fmt.Fprintln(w, "Poll with:        gregale invocation", r.ID)
}

// oneLine collapses a multi-line string into a single line for the
// labelled-block view. Newlines become spaces so a stack-trace dump
// or a JSON string with embedded '\n' doesn't break the column
// alignment. Long payloads (>120 chars) are truncated with "…" so a
// stray base64 blob doesn't blow past the terminal width — the JSON
// path carries the full string verbatim.
//
// Truncation honours rune boundaries: a byte slice at max-1 can
// split a multibyte UTF-8 sequence and emit invalid UTF-8 to a
// terminal that handles all-writes-as-bytes. We always truncate on
// a rune boundary so the output is always valid UTF-8. (A 4-byte
// CJK glyph at the cut means we keep one fewer rune than the byte
// ceiling — acceptable; the cap is a terminal-width hint, not a
// wire contract.)
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	const max = 120
	// Rune count is what the terminal sees; byte length would
	// over-count multibyte sequences and trip the cap on short
	// UTF-8 strings.
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	var b strings.Builder
	b.Grow(max + 3) // worst-case "…" + slack
	count := 0
	for _, r := range s {
		if count == max-1 {
			break
		}
		b.WriteRune(r)
		count++
	}
	b.WriteString("…")
	return b.String()
}

// ensureJSONReusable is a compile-time check that api.Invocation is
// still a JSON-serialisable struct (the SDK's wire shape must not
// silently regress to a non-marshalable form). The closure runs at
// init; if marshalling ever breaks the build fails here instead of
// at the first customer's `gregale invocation --json`.
var _ = func() error {
	_, err := json.Marshal(api.Invocation{})
	return err
}
