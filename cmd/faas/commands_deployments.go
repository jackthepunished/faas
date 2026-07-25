// Deployments list/get commands. Two top-level subcommands dispatched
// from main.go (cases `dispatchDeployments` and `deploymentSlugFallback`).
//
// Design notes (kept short so the handlers stay under the 50-line cap):
//
//   - `faas deployments` lists a single page (50 rows by default) so
//     the customer sees what just shipped without thinking about
//     pagination. `--limit N` and `--before CURSOR` thread through to
//     the API; `--all` walks every page via the existing
//     Client.ListDeploymentsAll helper (pkg/api/paging.go).
//
//   - JSON output for the list path is the *envelope* ({items,next_before})
//     rather than NDJSON per record. This is a deliberate break from
//     the apps/crons/keys convention; the endpoint is paginated and
//     dropping next_before is worse for automation. The singular
//     `faas deployment <id>` JSON path is the dotted envelope object
//     (one indented JSON object), matching AccountResponse / AppResponse.
//
//   - `faas deployment <id>` validates the 32-hex shape locally so
//     `--json` users get `validation_failed` instead of a 404. The
//     server enforces the same shape.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"regexp"

	"github.com/onebox-faas/faas/pkg/api"
)

// deploymentIDPattern enforces the 32-hex deployment id shape the API
// uses everywhere (DeploymentResponse.ID, also the path segment of
// /v1/deployments/{id} and the --deployment flag on `faas logs`).
// Local validation lets the CLI return a fast validation_failed error
// instead of a 404 round-trip — UX §3.3 "first error is the right one".
var deploymentIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)

// cmdDeployments implements `faas deployments [--limit N] [--before C] [--all]`.
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
		PrintUsage(os.Stderr, "usage: faas deployments [--limit N] [--before CURSOR] [--all]", "deployments")
		return 1
	}
	if *limit < 0 || *limit > 200 {
		PrintUsage(os.Stderr, "usage: faas deployments --limit N (0 < N <= 200)", "deployments")
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
		_, _ = fmt.Fprintln(os.Stdout, "Deploy one: `faas deploy --tarball path/to/source.tar.gz` (or `faas deploy --image <ref>`).")
		return 0
	}
	for _, d := range page.Items {
		fmt.Printf("%-32s %-32s %-12s %-10s %s\n", d.ID, d.AppID, d.Status, d.Kind, d.CreatedAt)
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
		_, _ = fmt.Fprintln(os.Stdout, "No deployments yet.")
		return 0
	}
	for _, d := range items {
		fmt.Printf("%-32s %-32s %-12s %-10s %s\n", d.ID, d.AppID, d.Status, d.Kind, d.CreatedAt)
	}
	return 0
}

// cmdDeployment implements `faas deployment <id>` (GET /v1/deployments/{id}).
// Mirrors the read branch of cmdApp (commands2.go:69) — single positional
// id, JSON single record, human multi-line detail block.
func cmdDeployment(args []string) int {
	if len(args) != 1 {
		PrintUsage(os.Stderr, "usage: faas deployment <id>", "deployment")
		return 1
	}
	id := args[0]
	if !deploymentIDPattern.MatchString(id) {
		PrintUsage(os.Stderr, "usage: faas deployment <id>   (id is 32 hex chars)", "deployment")
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
		return jsonOut(writeJSON(d))
	}
	fmt.Printf("%-14s %s\n", "id:", d.ID)
	fmt.Printf("%-14s %s\n", "app_id:", d.AppID)
	if d.BuildID != "" {
		fmt.Printf("%-14s %s\n", "build_id:", d.BuildID)
	}
	fmt.Printf("%-14s %s\n", "image_digest:", d.ImageDigest)
	fmt.Printf("%-14s %s\n", "kind:", d.Kind)
	fmt.Printf("%-14s %s\n", "status:", d.Status)
	fmt.Printf("%-14s %s\n", "created_at:", d.CreatedAt)
	if d.Error != "" {
		fmt.Printf("%-14s %s\n", "error:", d.Error)
	}
	if d.ErrorCode != "" {
		fmt.Printf("%-14s %s\n", "error_code:", d.ErrorCode)
	}
	return 0
}
