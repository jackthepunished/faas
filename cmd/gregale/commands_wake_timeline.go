// commands_wake_timeline.go — Tier D audit-gap close.
// `gregale wake-timeline <slug> <wake-id> [--since RFC3339] [--limit N] [--all]`.
// Mirrors cmdInvocationsGet (commands_invocations.go) for the read shape
// and cmdDeploymentsAll (commands_deployments.go:118) for the walker.
//
// Two positional args are required: <slug> + <wake-id>. --since / --limit
// thread through to the SDK; --all walks every page via the
// next_cursor / since loop (the SDK does not expose a ListWakeTimelineAll
// helper — the loop lives in the CLI).
//
// Output modes:
//   - --json (single page): writeJSON(resp) — preserves envelope so
//     `next_cursor` survives for follow-up `?since=` calls.
//   - --json (--all walker): NDJSON of WakeTimelineEvent records.
//   - human (single page): labelled header + one row per event.
//   - human (--all): same per-event rows streamed across pages; if a
//     follow-up page exists, prints `... more — pass --since <cursor>`.
//
// The response-side closed enum (`WakeTimelineEvent.Kind`, ≥20 wake.* values
// from pkg/events/wake.go) is intentionally NOT gated client-side on this
// read path — operators want to see whatever the server surfaces, including
// legacy non-wake.* kinds.

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
)

// wakeTimelineDefaultLimit mirrors the SDK's documented default. The
// handler rejects limit > 1000 explicitly (handlers_wake_timeline.go:114);
// we cap the CLI default at 50 to keep a single page readable in a
// terminal without scrolling, matching the deployments list path.
const wakeTimelineDefaultLimit = 50

// wakeTimelineMaxLimit mirrors the SDK's documented page-size cap. The
// handler rejects limit > 1000 (handlers_wake_timeline.go:114); we
// mirror 1000 as the CLI ceiling so an operator cannot blow past the
// server-side gate via a higher --limit (zero-latency failure).
const wakeTimelineMaxLimit = 1000

// cmdWakeTimeline implements `gregale wake-timeline <slug> <wake-id>
// [--since RFC3339] [--limit N] [--all]`. Issue #517 / PR-C / ADR-064.
// Mirrors cmdInvocationsGet (single positional read with optional
// flags) + cmdDeploymentsAll (cursor walker).
func cmdWakeTimeline(args []string) int {
	fs := flag.NewFlagSet("wake-timeline", flag.ContinueOnError)
	since := fs.String("since", "", "RFC3339 timestamp; rows with `at >= since` returned (cursor when paging)")
	limit := fs.Int("limit", wakeTimelineDefaultLimit, "page size (1..1000)")
	all := fs.Bool("all", false, "walk every page via next_cursor / since (ignores --limit for the call count)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 2 {
		PrintUsage(os.Stderr, "usage: gregale wake-timeline <slug> <wake-id> [--since RFC3339] [--limit N] [--all]", "wake-timeline")
		return 1
	}
	if *limit < 1 || *limit > wakeTimelineMaxLimit {
		return printErr("Invalid --limit", fmt.Errorf("--limit must be in [1, %d]; got %d", wakeTimelineMaxLimit, *limit))
	}
	if *since != "" {
		if _, err := time.Parse(time.RFC3339, *since); err != nil {
			return printErr("Invalid --since", fmt.Errorf("--since must be RFC 3339; got %q (%w)", *since, err))
		}
	}
	slug := fs.Arg(0)
	wakeID := fs.Arg(1)
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	ctx := context.Background()
	if *all {
		return cmdWakeTimelineAll(ctx, client, slug, wakeID, *limit)
	}
	page, err := client.ListWakeTimeline(ctx, slug, wakeID, *since, *limit)
	if err != nil {
		return printErr("Could not fetch wake timeline", err)
	}
	if jsonOutput {
		return jsonOut(writeJSON(page))
	}
	renderWakeTimelinePage(osStdout, page)
	if page.NextCursor != "" {
		_, _ = fmt.Fprintf(osStdout, "... more — pass --since %s\n", page.NextCursor)
	}
	return 0
}

// cmdWakeTimelineAll walks every page of the wake timeline by feeding
// the response's next_cursor back as the next request's `since`. Stops
// when next_cursor is empty or the server returns no events on the
// next page (defensive). Mirrors cmdDeploymentsAll for the loop
// shape but emits per-event rows so the operator sees a single
// chronological narrative across pages.
func cmdWakeTimelineAll(ctx context.Context, client *api.Client, slug, wakeID string, limit int) int {
	cursor := ""
	for {
		page, err := client.ListWakeTimeline(ctx, slug, wakeID, cursor, limit)
		if err != nil {
			return printErr("Could not fetch wake timeline", err)
		}
		if jsonOutput {
			for _, ev := range page.Events {
				if code := jsonOut(writeJSON(ev)); code != 0 {
					return code
				}
			}
		} else {
			renderWakeTimelinePage(osStdout, page)
		}
		if page.NextCursor == "" || len(page.Events) == 0 {
			return 0
		}
		cursor = page.NextCursor
	}
}

// renderWakeTimelinePage writes one page of events to w. Header row
// identifies the wake + page boundary; one row per event in the order
// the server returned them (at ASC — forward narrative). Mirrors the
// labelled-block convention of cmdAlertInfo (commands_alerts.go:195)
// rather than a fixed-width table because the `data` column carries
// heterogeneous JSON.
func renderWakeTimelinePage(w io.Writer, p api.WakeTimelineResponse) {
	_, _ = fmt.Fprintf(w, "wake %s app %s limit %d:\n", p.WakeID, p.AppID, p.Limit)
	for _, ev := range p.Events {
		_, _ = fmt.Fprintf(w, "  %s  %-9s  %s\n", ev.At, ev.Actor, ev.Kind)
	}
}
