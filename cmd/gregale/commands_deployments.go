// Deployments list/get commands. Two top-level subcommands dispatched
// from main.go (cases `dispatchDeployments` and `dispatchDeployment`).
//
// Design notes (kept short so the handlers stay under the 50-line cap):
//
//   - `gregale deployments` lists a single page (50 rows by default) so
//     the customer sees what just shipped without thinking about
//     pagination. `--limit N` and `--before CURSOR` thread through to
//     the API; `--all` walks every page via the existing
//     Client.ListDeploymentsAll helper (pkg/api/paging.go).
//
//   - JSON output for the list path is the *envelope* ({items,next_before})
//     rather than NDJSON per record. This is a deliberate break from
//     the apps/crons/keys convention; the endpoint is paginated and
//     dropping next_before is worse for automation. The singular
//     `gregale deployment <id>` JSON path is the dotted envelope object
//     (one indented JSON object), matching AccountResponse / AppResponse.
//
//   - `gregale deployment <id>` validates the 32-hex shape locally so
//     `--json` users get `validation_failed` instead of a 404. The
//     server enforces the same shape.
//
// Field set tracks pkg/api/dto.go:86 DeploymentResponse verbatim; update
// the human table when the DTO grows (e.g. commit_sha when function
// runners land). The OpenAPI example block is currently stale against
// the DTO; spec cleanup is a separate PR.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"

	"github.com/onebox-faas/faas/pkg/api"
)

// deploymentIDPattern enforces the 32-hex deployment id shape the API
// uses everywhere (DeploymentResponse.ID, also the path segment of
// /v1/deployments/{id} and the --deployment flag on `gregale logs`).
// Local validation lets the CLI return a fast validation_failed error
// instead of a 404 round-trip — UX §3.3 "first error is the right one".
var deploymentIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)

// deploymentRowFmt is the human list-table column layout. Widths assume
// 32-hex deployment ids (32 chars), 32-hex app ids (32 chars), short
// status strings ("succeeded" / "failed" / "rolling_out"), and the
// app/function kind discriminator. If the DTO grows an id longer than
// 32 hex chars this layout has to shift; covered by
// TestRenderDeploymentRow pin on the column count.
const deploymentRowFmt = "%-32s %-32s %-12s %-10s %s\n"

// renderDeploymentRow writes one deployment row to w. The fmt.Printf
// inside writes to os.Stdout by default; tests that need the rendered
// row in a buffer must use the osStdout package seam (see
// commands_test.go for the equivalent helper). The Fprintf return is
// intentionally discarded: writer failures (closed pipe, broken TTY)
// are unrecoverable here, matching writeStatus / output.go's convention.
func renderDeploymentRow(w io.Writer, d api.DeploymentResponse) {
	_, _ = fmt.Fprintf(w, deploymentRowFmt, d.ID, d.AppID, d.Status, d.Kind, d.CreatedAt)
}

// cmdDeployments implements `gregale deployments [--limit N] [--before C] [--all]`.
// Mirrors cmdApps (commands.go:251) except pagination is exposed.
// Wire shape: GET /v1/deployments (paginated; cursor=before, limit=limit).
func cmdDeployments(args []string) int {
	fs := flag.NewFlagSet("deployments", flag.ContinueOnError)
	limit := fs.Int("limit", 50, "page size (1-200)")
	before := fs.String("before", "", "pagination cursor (RFC3339Nano)")
	all := fs.Bool("all", false, "walk every page (ignores --limit/--before)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 0 {
		PrintUsage(os.Stderr, "usage: gregale deployments [--limit N] [--before CURSOR] [--all]", "deployments")
		return 1
	}
	if *limit < 0 || *limit > 200 {
		PrintUsage(os.Stderr, "usage: gregale deployments --limit N (0 < N <= 200)", "deployments")
		return 1
	}
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	ctx := context.Background()
	if *all {
		return cmdDeploymentsAll(ctx, client)
	}
	page, err := client.ListDeployments(ctx, *before, *limit)
	if err != nil {
		return printErr("Request failed", err)
	}
	if jsonOutput {
		// Envelope (not NDJSON) so `next_before` survives; see file header.
		return jsonOut(writeJSON(page))
	}
	if len(page.Items) == 0 {
		_, _ = fmt.Fprintln(osStdout, "No deployments yet.")
		_, _ = fmt.Fprintln(osStdout, "Deploy one: `gregale deploy --tarball path/to/source.tar.gz` (or `gregale deploy --image <ref>`).")
		return 0
	}
	for _, d := range page.Items {
		renderDeploymentRow(osStdout, d)
	}
	if page.NextBefore != "" {
		_, _ = fmt.Fprintf(osStdout, "... more — pass --before %s\n", page.NextBefore)
	}
	return 0
}

// cmdDeploymentsAll walks every page via the SDK helper and renders the
// full list. Refuses to share a single envelope with the one-page path
// (no `next_before` to surface), so JSON output is the bare slice —
// matching how apps/crons/keys emit NDJSON for non-paginated lists.
func cmdDeploymentsAll(ctx context.Context, client *api.Client) int {
	items, err := client.ListDeploymentsAll(ctx)
	if err != nil {
		return printErr("Request failed", err)
	}
	if jsonOutput {
		return jsonOut(writeJSON(items))
	}
	if len(items) == 0 {
		_, _ = fmt.Fprintln(osStdout, "No deployments yet.")
		return 0
	}
	for _, d := range items {
		renderDeploymentRow(osStdout, d)
	}
	return 0
}

// cmdDeployment dispatches `gregale deployment <verb> ...` to either
// the legacy singular GET (`gregale deployment <id> [--show-scan]`) or
// the Tier D mutator `gregale deployment set-min-instances <id> --min N`.
// The 3-word verb shape mirrors cmdWebhookRotateSecret (commands_webhooks.go:361).
func cmdDeployment(args []string) int {
	if len(args) == 0 {
		PrintUsage(os.Stderr, "usage: gregale deployment <id> [--show-scan] | gregale deployment set-min-instances <id> --min N", "deployment")
		return 1
	}
	switch args[0] {
	case "set-min-instances":
		return cmdDeploymentSetMinInstances(args[1:])
	}
	return cmdDeploymentGet(args)
}

// cmdDeploymentGet implements `gregale deployment <id> [--show-scan]`
// (GET /v1/deployments/{id}, plus GET /v1/deployments/{id}/scan
// when --show-scan is set). Mirrors the read branch of cmdApp
// (commands2.go:69) — single positional id, JSON single record,
// human multi-line detail block.
//
// --show-scan is a flag (not a separate `gregale scan <id>`
// subcommand) because `gregale scan` is already taken by the
// Phase 3 repo-decomposition dry-run surface at
// cmd/gregale/commands_decompose.go:49. The flag-vs-subcommand
// split is the smallest-mess resolution of the name collision.
func cmdDeploymentGet(args []string) int {
	fs := flag.NewFlagSet("deployment", flag.ContinueOnError)
	showScan := fs.Bool("show-scan", false, "fetch + print the per-deploy grype scan payload (GET /v1/deployments/{id}/scan)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 1 {
		PrintUsage(os.Stderr, "usage: gregale deployment <id> [--show-scan]", "deployment")
		return 1
	}
	id := fs.Arg(0)
	if !deploymentIDPattern.MatchString(id) {
		PrintUsage(os.Stderr, "usage: gregale deployment <id> [--show-scan]   (id is 32 hex chars)", "deployment")
		return 1
	}
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	d, err := client.GetDeployment(context.Background(), id)
	if err != nil {
		return printErr("Could not fetch deployment", err)
	}
	if jsonOutput {
		if !*showScan {
			return jsonOut(writeJSON(d))
		}
		sc, scanErr := client.GetDeploymentScan(context.Background(), id)
		if scanErr != nil {
			// Non-fatal in JSON mode: return the deployment as-is so
			// an operator script never silently drops the payload.
			// The CLI exit code still carries the error.
			_ = scanErr
			return jsonOut(writeJSON(d))
		}
		type deploymentWithScan struct {
			Deployment any `json:"deployment"`
			Scan       any `json:"scan"`
		}
		return jsonOut(writeJSON(deploymentWithScan{Deployment: d, Scan: sc}))
	}
	_, _ = fmt.Fprintf(osStdout, "%-14s %s\n", "id:", d.ID)
	_, _ = fmt.Fprintf(osStdout, "%-14s %s\n", "app_id:", d.AppID)
	if d.BuildID != "" {
		_, _ = fmt.Fprintf(osStdout, "%-14s %s\n", "build_id:", d.BuildID)
	}
	_, _ = fmt.Fprintf(osStdout, "%-14s %s\n", "image_digest:", d.ImageDigest)
	_, _ = fmt.Fprintf(osStdout, "%-14s %s\n", "kind:", d.Kind)
	_, _ = fmt.Fprintf(osStdout, "%-14s %s\n", "status:", d.Status)
	_, _ = fmt.Fprintf(osStdout, "%-14s %s\n", "created_at:", d.CreatedAt)
	if d.Error != "" {
		_, _ = fmt.Fprintf(osStdout, "%-14s %s\n", "error:", d.Error)
	}
	if d.ErrorCode != "" {
		_, _ = fmt.Fprintf(osStdout, "%-14s %s\n", "error_code:", d.ErrorCode)
	}
	if *showScan {
		sc, scanErr := client.GetDeploymentScan(context.Background(), id)
		if scanErr != nil {
			_, _ = fmt.Fprintf(osStdout, "%-14s (scan unavailable: %v)\n", "scan:", scanErr)
		} else {
			_, _ = fmt.Fprintf(osStdout, "\n%-14s %s\n", "scan_status:", sc.Status)
			if sc.ScannedAt != "" {
				_, _ = fmt.Fprintf(osStdout, "%-14s %s\n", "scanned_at:", sc.ScannedAt)
			}
			if sc.ScannerVersion != "" {
				_, _ = fmt.Fprintf(osStdout, "%-14s %s\n", "scanner_version:", sc.ScannerVersion)
			}
			if sc.ImageDigest != "" {
				_, _ = fmt.Fprintf(osStdout, "%-14s %s\n", "image_digest:", sc.ImageDigest)
			}
			_, _ = fmt.Fprintf(osStdout, "%-14s C=%d H=%d M=%d L=%d U=%d\n", "severity_counts:", sc.SeverityCounts.Critical, sc.SeverityCounts.High, sc.SeverityCounts.Medium, sc.SeverityCounts.Low, sc.SeverityCounts.Unknown)
			if sc.Error != "" {
				_, _ = fmt.Fprintf(osStdout, "%-14s %s\n", "scan_error:", sc.Error)
			}
			if len(sc.Vulnerabilities) > 0 {
				_, _ = fmt.Fprintf(osStdout, "%-14s %d\n", "vulnerabilities:", len(sc.Vulnerabilities))
				for _, v := range sc.Vulnerabilities {
					_, _ = fmt.Fprintf(osStdout, "  - %s [%s] %s %s (fixed in %s)\n", v.ID, v.Severity, v.Package, v.Version, v.FixedIn)
				}
			}
		}
	}
	return 0
}

// cmdDeploymentSetMinInstances implements `gregale deployment
// set-min-instances <id> --min N` (PATCH /v1/deployments/{id}, issue #557,
// ADR-072 — per-deployment cold-wake floor override).
//
// --min N sets min_instances on the deployment. The server treats
// min_instances=0 as "inherit parent app floor" (per
// pkg/api/client.go:417-419 — the SDK emits {"min_instances":0} verbatim,
// and handlers_ext.go:1051-1093 validates against acct.Plan.MaxMinInstances).
//
// The 3-word verb shape mirrors cmdWebhookRotateSecret
// (commands_webhooks.go:361) — grep-friendly and matches the kebab-case
// surface in api/openapi.yaml.
//
// Local --min >= 0 gate runs before authedClient() so a CLI typo costs
// zero latency (mirrors validateAlertClosedSets, commands_alerts.go:172).
func cmdDeploymentSetMinInstances(args []string) int {
	// splitArgsForFlags: Go's flag.Parse halts at the first non-flag
	// positional, so `gregale deployment set-min-instances <id> --min 5`
	// would silently drop --min 5 and send min_instances:0 to the
	// server (resetting the cold-wake floor). The reorder helper
	// pulls --min to the front so the parser sees it. Mirrors
	// cmdDelayedTaskAdd (commands_delayed_task.go:118).
	flags, pos := splitArgsForFlags(args)
	fs := flag.NewFlagSet("deployment set-min-instances", flag.ContinueOnError)
	min := fs.Int("min", 0, "min_instances floor (>= 0; 0 inherits the parent app floor)")
	if err := fs.Parse(flags); err != nil {
		return 1
	}
	if len(pos) != 1 {
		PrintUsage(os.Stderr, "usage: gregale deployment set-min-instances <id> --min N", "deployment")
		return 1
	}
	if *min < 0 {
		return printErr("Invalid --min", fmt.Errorf("--min must be >= 0; got %d", *min))
	}
	id := pos[0]
	if !deploymentIDPattern.MatchString(id) {
		PrintUsage(os.Stderr, "usage: gregale deployment set-min-instances <id> --min N   (id is 32 hex chars)", "deployment")
		return 1
	}
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	d, err := client.PatchDeployment(context.Background(), id, api.UpdateDeploymentRequest{MinInstances: min})
	if err != nil {
		return printErr("Update failed", err)
	}
	if jsonOutput {
		return jsonOut(writeJSON(d))
	}
	PrintOK(osStdout, "Deployment %s updated.", d.ID)
	_, _ = fmt.Fprintf(osStdout, "  min_instances: %d\n", d.MinInstances)
	return 0
}
