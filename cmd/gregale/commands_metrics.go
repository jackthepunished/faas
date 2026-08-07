// commands_metrics.go — Move 1 PR-A: customer-facing CLI twin for
// GET /v1/apps/{slug}/metrics (issue #273 / ADR-042).
//
// The HTTP endpoint has shipped since the dashboard work (cmd/apid
// renders the same data at /dashboard/apps/{slug}), but the CLI had
// no entry point. A customer in their editor hits a 30-second
// latency regression and has to context-switch into a browser tab;
// this command lands that read in the terminal where the rest of
// the debugging already happens.
//
// Output shape mirrors the dashboard panel: range / as_of / source
// header, then a labelled block (not a table — the values are
// heterogeneous widths and labels are clearer than columns here).
//
// `--json` follows the global jsonOutput convention (json_flag.go):
// NDJSON for slices, indented JSON for scalars. The DTO is
// AppMetricsResponse (pkg/api/dto.go:1087); no client-side reshaping.
//
// Tier C extension: --account flips to GET /v1/apps/metrics (the
// account-wide rollup, issue #393) and renders the per-slug map.
// --account is mutually exclusive with the positional <slug>.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/appmetrics"
)

// metricsCmdUsage is the top-of-failure-line shown for `gregale metrics`
// errors. Mirrors PrintUsage's docs URL convention (output.go:144) so
// the line carries the stable docs site pointer.
const metricsCmdUsage = "usage: gregale metrics <slug> [--range 5m] | --account [--range 5m]"

// metricsCmdDocsTopic is the docs topic slug appended to docsURLBase
// when PrintUsage emits the trailing "Docs:" row. Keeps the CLI's
// help line stable across command additions.
const metricsCmdDocsTopic = "metrics"

// cmdMetrics implements `gregale metrics <slug> [--range 5m]` and
// `gregale metrics --account [--range 5m]`. Mirrors the read shape
// of cmdDeployment (commands_deployments.go:139) — single positional
// slug + a few flags, JSON single record, human multi-line detail
// block. --account (Tier C) hits the account-wide rollup endpoint
// instead and renders one labelled block per app.
func cmdMetrics(args []string) int {
	fs := flag.NewFlagSet("metrics", flag.ContinueOnError)
	rng := fs.String("range", "5m", "time window (5m, 15m, 1h, 6h, 24h)")
	account := fs.Bool("account", false, "account-wide rollup (GET /v1/apps/metrics) — mutually exclusive with <slug>")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *account && fs.NArg() != 0 {
		PrintUsage(os.Stderr, metricsCmdUsage, metricsCmdDocsTopic)
		return 1
	}
	if !*account && fs.NArg() != 1 {
		PrintUsage(os.Stderr, metricsCmdUsage, metricsCmdDocsTopic)
		return 1
	}
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	if *account {
		m, err := client.GetAppsMetrics(context.Background(), *rng)
		if err != nil {
			return printErr("Could not fetch metrics", err)
		}
		if jsonOutput {
			return jsonOut(writeJSON(m))
		}
		renderAppsMetrics(osStdout, m)
		return 0
	}
	slug := fs.Arg(0)
	m, err := client.GetAppMetrics(context.Background(), slug, *rng)
	if err != nil {
		return printErr("Could not fetch metrics", err)
	}
	if jsonOutput {
		return jsonOut(writeJSON(m))
	}
	renderAppMetrics(osStdout, m)
	return 0
}

// renderAppMetrics writes the human-mode labelled block for an
// AppMetricsResponse. Mirrors the dashboard panel (cmd/apid renders
// the same fields) so a customer toggling between terminal and
// browser sees the same numbers.
//
// When Source is "degraded: <reason>" we render a one-line warning
// before the values so the customer understands the zeroes are
// real (Prometheus isn't reachable), not a bug.
func renderAppMetrics(w io.Writer, m api.AppMetricsResponse) {
	if m.Source != "" && m.Source != appmetrics.SourcePrometheus {
		_, _ = fmt.Fprintf(w, "Note: source=%s (values below are zero — Prometheus is unavailable)\n", m.Source)
	}
	_, _ = fmt.Fprintf(w, "App:        %s\n", m.AppID)
	_, _ = fmt.Fprintf(w, "Range:      %s\n", m.Range)
	if m.AsOf != "" {
		_, _ = fmt.Fprintf(w, "As of:      %s\n", m.AsOf)
	}
	if m.Source != "" {
		_, _ = fmt.Fprintf(w, "Source:     %s\n", m.Source)
	}
	_, _ = fmt.Fprintf(w, "Requests:   %d (in window)\n", m.RequestCount)
	_, _ = fmt.Fprintf(w, "Latency:    p50=%.1fms p95=%.1fms p99=%.1fms\n", m.LatencyP50MS, m.LatencyP95MS, m.LatencyP99MS)
	_, _ = fmt.Fprintf(w, "Error rate: %.2f%%\n", m.ErrorRatePct)
	_, _ = fmt.Fprintf(w, "Cold boot:  %.2f%%\n", m.ColdStartPct)
	if m.WakeP95MS > 0 {
		_, _ = fmt.Fprintf(w, "Wake p95:   %.0fms (fleet-wide)\n", m.WakeP95MS)
	}
}

// renderAppsMetrics writes the human-mode labelled block for an
// account-wide AppsMetricsResponse (issue #393). One range/as_of/
// source header, then one App: <slug> block per row in the Apps
// map. Sort order follows the dashboard's table view — alphabetical
// by slug — so terminal output is stable across calls.
//
// When Source is degraded we render the warning once at the top so
// every zero the operator sees below is interpreted correctly
// (Prometheus isn't reachable, not a customer app bug).
func renderAppsMetrics(w io.Writer, m api.AppsMetricsResponse) {
	if m.Source != "" && m.Source != appmetrics.SourcePrometheus {
		_, _ = fmt.Fprintf(w, "Note: source=%s (values below are zero — Prometheus is unavailable)\n", m.Source)
	}
	_, _ = fmt.Fprintf(w, "Range:      %s\n", m.Range)
	if m.AsOf != "" {
		_, _ = fmt.Fprintf(w, "As of:      %s\n", m.AsOf)
	}
	if m.Source != "" {
		_, _ = fmt.Fprintf(w, "Source:     %s\n", m.Source)
	}
	if len(m.Apps) == 0 {
		_, _ = fmt.Fprintln(w, "(no apps with metrics in window)")
		return
	}
	slugs := make([]string, 0, len(m.Apps))
	for s := range m.Apps {
		slugs = append(slugs, s)
	}
	sortStrings(slugs)
	for _, s := range slugs {
		row := m.Apps[s]
		_, _ = fmt.Fprintf(w, "\nApp:        %s\n", s)
		_, _ = fmt.Fprintf(w, "  Requests:   %d\n", row.RequestCount)
		_, _ = fmt.Fprintf(w, "  Latency:    p50=%.1fms p95=%.1fms p99=%.1fms\n", row.LatencyP50MS, row.LatencyP95MS, row.LatencyP99MS)
		_, _ = fmt.Fprintf(w, "  Error rate: %.2f%%\n", row.ErrorRatePct)
		_, _ = fmt.Fprintf(w, "  Cold boot:  %.2f%%\n", row.ColdStartPct)
	}
}

// sortStrings delegates to stdlib sort.Strings (pdqsort, O(n log n)).
// Extracted so renderAppsMetrics reads cleanly — the wrapper is one
// line vs. the call site's naked sort.Strings(xs) which would scan
// the diff for "why is sort imported here?"
func sortStrings(xs []string) { sort.Strings(xs) }
