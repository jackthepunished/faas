// commands_audit_events.go — `gregale audit-events` operator/customer CLI
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
// --verbose (Move 1 PR-A) switches the human-mode renderer to a
// 5-column expanded view: instance | count | paths | sample_pid |
// last_ts. The expanded columns are derived by decoding the audit
// row's Data JSON (the shape cmd/apid/advisory_receiver.go writes:
// {instance, app_id, count, events: [{path, mask, pid,
// ts_unix_ms}]}). Non-stateless rows fall back to the default
// 4-column rendering so the flag is always safe to pass.
//
// Auth: the route is s.auth + requireScope(api.ScopesReadSurface) —
// the same gating as the rest of the read surface. A session-cookie
// principal (Key == nil) implicitly carries admin scope per
// principalHasScope, so a dashboard customer can read their own log
// without holding an API key.

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"
)

// cmdAuditEvents implements `gregale audit-events [--kind-prefix P]
// [--app-id <uuid>] [--since RFC3339] [--limit N]
// [--include-anonymous] [--verbose]`. Returns 0 on success, 2 on
// operator error (bad flags), 1 on transport / 5xx.
func cmdAuditEvents(args []string) int {
	fs := flag.NewFlagSet("audit-events", flag.ContinueOnError)
	kindPrefix := fs.String("kind-prefix", "", "filter by `kind` prefix (e.g. stateless.advisory)")
	appID := fs.String("app-id", "", "filter to one app's events (matches data.app_id)")
	since := fs.String("since", "", "RFC 3339 lower bound on `at`")
	limit := fs.Int("limit", 50, "max rows (1..100; server caps at 100)")
	includeAnon := fs.Bool("include-anonymous", false, "also surface subject=NULL rows (operator post-mortem)")
	verbose := fs.Bool("verbose", false, "expand stateless.advisory rows into 5 columns (instance, count, paths, sample_pid, last_ts)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: gregale audit-events [--kind-prefix P] [--app-id <uuid>] [--since RFC3339] [--limit N] [--include-anonymous] [--verbose]")
		return 2
	}
	if *since != "" {
		if _, err := time.Parse(time.RFC3339, *since); err != nil {
			fmt.Fprintf(os.Stderr, "gregale: --since must be RFC 3339 (e.g. 2026-07-25T00:00:00Z): %v\n", err)
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
		if *verbose && strings.HasPrefix(e.Kind, "stateless.advisory") {
			renderVerboseAuditRow(e.At, e.Actor, e.Kind, subject, e.Data)
			continue
		}
		fmt.Printf("%s\t%s\t%s\t%s\n", e.At, e.Actor, e.Kind, subject)
	}
	return 0
}

// advisoryBatch is the decoded shape of a stateless.advisory audit
// row's Data field. Mirrors cmd/apid/advisory_receiver.go's emit
// (events slice of {path, mask, pid, ts_unix_ms}). Kept here as a
// private type so the CLI doesn't grow a dependency on the apid
// package — the shape is owned by the audit row's JSON contract.
type advisoryBatch struct {
	Instance string `json:"instance"`
	AppID    string `json:"app_id"`
	Count    int    `json:"count"`
	Events   []struct {
		Path     string   `json:"path"`
		Mask     []string `json:"mask"`
		PID      int      `json:"pid"`
		TSUnixMS int64    `json:"ts_unix_ms"`
	} `json:"events"`
}

// renderVerboseAuditRow prints the 5-column expanded view of a
// stateless.advisory row. The columns are:
//
//	instance | count | paths | sample_pid | last_ts
//
// Multiple paths are joined with comma (most rows have one path;
// multi-path rows are the interesting "storm" case the operator
// is looking for). Decoding failures fall back to the 4-col render
// so a future data-shape drift in advisory_receiver.go can't crash
// the CLI.
func renderVerboseAuditRow(at, actor, kind, subject string, raw json.RawMessage) {
	var b advisoryBatch
	if err := json.Unmarshal(raw, &b); err != nil {
		fmt.Printf("%s\t%s\t%s\t%s\n", at, actor, kind, subject)
		return
	}
	paths := make([]string, 0, len(b.Events))
	var samplePID int
	var lastTS int64
	for _, e := range b.Events {
		paths = append(paths, e.Path)
		if e.PID != 0 {
			samplePID = e.PID
		}
		if e.TSUnixMS > lastTS {
			lastTS = e.TSUnixMS
		}
	}
	var lastTSStr string
	if lastTS > 0 {
		lastTSStr = time.UnixMilli(lastTS).UTC().Format(time.RFC3339)
	} else {
		lastTSStr = "-"
	}
	pidStr := "-"
	if samplePID != 0 {
		pidStr = fmt.Sprintf("%d", samplePID)
	}
	pathsStr := "-"
	if len(paths) > 0 {
		pathsStr = strings.Join(paths, ",")
	}
	instance := b.Instance
	if instance == "" {
		instance = subject
	}
	fmt.Printf("%s\t%s\t%d\t%s\t%s\t%s\n", at, instance, b.Count, pathsStr, pidStr, lastTSStr)
}
