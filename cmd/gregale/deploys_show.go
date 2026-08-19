// `gregale deploys show <id>` — read-only post-stream stage summary
// (ADR-117 companion to `gregale deploy --repo OWNER/NAME`).
//
// The live deploy path emits `event: stage` SSE frames inside
// GET /v1/deployments/{id}/logs while the deploy is running. Once the
// stream closes the terminal status (live / failed / superseded) is
// the only thing left visible to the operator. This command reads
// the closed 6-stage `deployments.stage_state` jsonb column via
// GET /v1/deployments/{id}/stages and renders it as a static block
// the operator can paste into a post-mortem or hand to support.
//
// Why a NEW top-level verb (`deploys`, not `deployment <id> show`):
//
//   - The singular `deployment <id>` verb is already a flat GET with
//     flag-shaped drill-downs (--show-scan, --show-secret-scan). A
//     sub-subcommand (`deployment <id> show --stages`) would force a
//     new subcommand layer on top of the existing positional-id parse
//     and split the human-table formatter across two files.
//   - The plural `deployments` verb is the paginated list. Adding a
//     `show` subcommand there would shadow `deployments` from being
//     usable as a bare list (today's `gregale deployments` returns the
//     page; a `show` subcommand would force `deployments list`).
//   - `deploys` is the noun-form cluster verb (mirrors `apps`/
//     `inspect`); it's a fresh entry in the usage block + dispatch
//     table, no shadowing.
//
// Scope is intentionally narrow:
//
//   - ONE positional arg, the 32-hex deployment id (same shape
//     as `deployment <id>`; same `deploymentIDPattern` regex gate).
//   - ONE subcommand today (`show`). Future read-only drill-downs
//     (timeline, events) land as siblings here, not as flags on
//     `deployment <id>`.
//   - NO `--follow` flag (decision recorded in
//     `cmd/gregale/deploy_stages.go:236` — `renderDeploySummary` is
//     a static post-stream renderer). Live stream lives in
//     `gregale deploy` itself.
//
// The wire shape comes back as raw `json.RawMessage` (the SDK method
// cannot import `pkg/state` directly because `pkg/state/memstore.go`
// imports `pkg/api` — see `pkg/api/client.go` GetDeploymentStages doc).
// We unmarshal into `state.StageState` here where the import direction
// is allowed (`cmd/gregale` already imports both packages).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/onebox-faas/faas/pkg/state"
)

// cmdDeploysShow implements `gregale deploys show <id> [--json]`.
//
// Wire call: GET /v1/deployments/{id}/stages (returns the raw
// deployments.stage_state jsonb verbatim; CLI is the typed-shape owner).
//
// Render path:
//   - --json (or FAAS_JSON=1)        → indented JSON of stage_state.
//   - stdout is TTY + no --json      → closed 6-row block via
//     renderDeploySummary.
//   - stdout is pipe / NO_COLOR set  → same closed 6-row block
//     (no ANSI redraw, just plain
//     print); output.Enabled() is
//     the single source of truth.
func cmdDeploysShow(args []string) int {
	fs := flag.NewFlagSet("deploys show", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 1 {
		PrintUsage(os.Stderr, "usage: gregale deploys show <id>", "deploys")
		return 1
	}
	id := fs.Arg(0)
	if !deploymentIDPattern.MatchString(id) {
		PrintUsage(os.Stderr, "usage: gregale deploys show <id>   (id is 32 hex chars)", "deploys")
		return 1
	}
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	ctx := context.Background()
	raw, err := client.GetDeploymentStages(ctx, id)
	if err != nil {
		return printErr("Could not fetch deployment stages", err)
	}
	var ss state.StageState
	if err := json.Unmarshal(raw, &ss); err != nil {
		// Should never happen — apid re-emits the raw jsonb that the
		// column CHECK constraint already validated. If it does, the
		// server and the CLI typed struct drifted apart (the column
		// gained a field the CLI doesn't know). Surface as 1 + a
		// clear error so the operator can paste the raw bytes back
		// to engineering.
		return printErr("Could not decode stage_state (CLI/server shape drift?)", err)
	}
	if jsonOutput {
		return jsonOut(writeJSON(ss))
	}
	// We don't know the terminal status without a second round-trip
	// (GET /v1/deployments/{id}); render the post-stream summary
	// with an empty status. renderDeploySummary handles the
	// status=="live" / "failed" branches via empty-string short
	// circuits so this still renders correctly for in-flight
	// deployments (footer prints "<ts>" but no "live since").
	if err := renderDeploySummary(osStdout, ss, "", time.Time{}); err != nil {
		// Render failures (closed-set drift, broken pipe) are
		// logged at WARN — the API call succeeded so the operator
		// can re-run with --json to get the raw bytes back. Exit
		// 0 because the wire call was authoritative.
		_, _ = fmt.Fprintf(os.Stderr, "warning: stage summary render failed: %v\n", err)
	}
	return 0
}

// cmdDeploys is the dispatcher for the `deploys` top-level verb.
//
// Today's only subcommand is `show` (this file). Future read-only
// drill-downs (timeline, events, artifacts) land here as sibling
// switch arms — NOT as flags on `gregale deployment <id>`, which
// already has --show-scan / --show-secret-scan / set-min-instances
// and would otherwise grow a third surface.
//
// The shape mirrors cmdDeployment (commands_deployments.go:140): a
// switch on args[0] with a help/usage fallthrough when args is empty
// or the verb is unknown.
func cmdDeploys(args []string) int {
	if len(args) == 0 {
		PrintUsage(os.Stderr, "usage: gregale deploys <subcommand> [flags]   (subcommands: show)", "deploys")
		return 1
	}
	switch args[0] {
	case "show":
		return cmdDeploysShow(args[1:])
	}
	PrintUsage(os.Stderr, "usage: gregale deploys <subcommand> [flags]   (subcommands: show)", "deploys")
	return 1
}
