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
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/onebox-faas/faas/pkg/api"
)

// metricsCmdUsage is the top-of-failure-line shown for `faas metrics`
// errors. Mirrors PrintUsage's docs URL convention (output.go:144) so
// the line carries the stable docs site pointer.
const metricsCmdUsage = "usage: faas metrics <slug> [--range 5m]"

// metricsCmdDocsTopic is the docs topic slug appended to docsURLBase
// when PrintUsage emits the trailing "Docs:" row. Keeps the CLI's
// help line stable across command additions.
const metricsCmdDocsTopic = "metrics"

// cmdMetrics implements `faas metrics <slug> [--range 5m]`. Mirrors
// the read shape of cmdDeployment (commands_deployments.go:139) —
// single positional slug + a few flags, JSON single record, human
// multi-line detail block.
func cmdMetrics(args []string) int {
	fs := flag.NewFlagSet("metrics", flag.ContinueOnError)
	rng := fs.String("range", "5m", "time window (5m, 15m, 1h, 6h, 24h)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 1 {
		PrintUsage(os.Stderr, metricsCmdUsage, metricsCmdDocsTopic)
		return 1
	}
	slug := fs.Arg(0)
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
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
	if m.Source != "" && m.Source != "prometheus" {
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
