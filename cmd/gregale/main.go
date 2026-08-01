// Command gregale is the customer-facing CLI and the primary interface to the
// platform (docs/faas_ux_spec.md §3). Everything the platform does is
// possible from here.
//
// Exit codes follow docs/faas_ux_spec.md §3.2: 0 ok, 1 user error, 2 auth,
// 3 platform/infra. See also the brand-residue sweep that landed in the
// same PR as the rename — every string in this file should say `gregale`,
// not `faas`, and any new dispatcher arm must add a matching entry to the
// usage block below so `gregale help` lists it.
package main

import (
	"fmt"
	"os"

	"github.com/onebox-faas/faas/pkg/wire"
)

// docsURL is the canonical link printed at the bottom of the usage string.
// Computed (not a const) so the tripwire that bans DOMAIN-shaped literals
// in source keeps working; the only host that surfaces in the binary is
// wire.DocsHost.
var docsURL = "https://" + wire.DocsHost

var usage = `gregale — deploy apps and functions that scale to zero.

Usage:
  gregale <command> [flags]

Commands:
  admin        Operator-only billing ops (admin credit --reason <text> <uuid> <cents>)
  audit-events Audit-log query (--kind-prefix X --include-anonymous)
  apps         List your apps
  apps ls      Alias for 'gregale apps'
  apps -q      Delete an app
  app          Get/update one app (gregale app <slug> [scale|rename <new>|--ram N|…])
  billing      Manage billing (gregale billing portal)
  build        Build provenance (build provenance <id>)
  connect      Connect a third-party service (github)
  crons        Manage scheduled requests
  dashboard    Open the account dashboard in your browser
  deployments  List deployments (--limit N | --before C | --all)
  deployment   Get one deployment (<id>)
  deploy       Deploy (--image REF | --tarball PATH | --repo OWNER/NAME | --template NAME)
  domains      Manage custom domains
  env          Pull/push .env <-> sealed secrets (--app <slug>)
  host-age     Operator host.age rotation (host-age init|rotate|status|prune-previous)
  init         Scaffold a reference project from a built-in template (--template NAME --path DIR [--deploy])
  invoices     List issued invoices
  keys         Manage API keys
  login        Authenticate this machine (--token for CI)
  logout       Remove the stored token
  logs         Tail app or deployment logs (--follow)
  metrics      Per-app request / latency / cold-boot metrics (gregale metrics <slug> [--range 5m])
  open         Open the app's URL (or its dashboard page) in your browser
  park         Park an app cold (kill all live instances)
  plan         Change plan (free|hobby|pro|scale)
  ps           Show live instances + state for an app
  queue        Inspect the wake-queue depth
  rollback     Re-promote the previous deployment
  secrets      Manage env secrets on an app (--app <slug>)
  sign-keys    Provision the cosign sign keypair (operator; --sign-key / --verify-key)
  status       Personal SLO numbers (availability, wake p95, build success)
  tail         Live tail of the unified event stream (--follow)
  usage        Show this month's usage (gregale usage [--month YYYY-MM])
  usage summary  Account-wide usage roll-up (gregale usage summary [--month YYYY-MM])
  version      Print the CLI version
  wake         Wake a parked app (pulls out of snapshot)
  whoami       Show the authenticated account

Run 'gregale <command> --help' for command details.

Global flags:
  --json         Machine-readable output on every command. Slices emit
                 NDJSON (one JSON object per line, jq -c '.'); scalars
                 emit indented JSON; errors print raw RFC 7807 to stderr.
                 Equivalent env: FAAS_JSON=1. Negate with --json=false.
Docs: ` + docsURL + `
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	// Issue #64 D1: every command accepts --json (top-level). Strip
	// it before dispatch and set jsonOutput so per-command printers
	// switch to NDJSON/indented JSON. FAAS_JSON=1 env also works.
	args = applyJSONFlag(args)
	if len(args) == 0 {
		fmt.Print(usage)
		return 0
	}
	switch args[0] {
	case "version", "--version", "-v":
		// `gregale version --help` prints usage + docs link; bare
		// `gregale version foo` still prints the version string (POSIX
		// convention — git does the same).
		if len(args) > 1 && (args[1] == "--help" || args[1] == "-h") {
			PrintUsage(os.Stderr, "usage: gregale version", "version")
			return 0
		}
		fmt.Printf("gregale %s\n", wire.Version)
		return 0
	case "help", "--help", "-h":
		fmt.Print(usage)
		return 0
	case "login":
		return cmdLogin(args[1:])
	case "logout":
		if len(args) > 1 {
			PrintUsage(os.Stderr, "usage: gregale logout", "auth")
			return 1
		}
		return cmdLogout()
	case "whoami":
		if len(args) > 1 {
			PrintUsage(os.Stderr, "usage: gregale whoami", "auth")
			return 1
		}
		return cmdWhoami()
	case "deploy":
		return cmdDeployTarball(args[1:])
	case "scan":
		// Phase 3 (repo decomposition) — dry-run entry point. Prints
		// the plan as a table or --json, never writes. The
		// transactional apply path lives in cmdDeployTarball when
		// --yes/--json/--only/--project-slug are set.
		return cmdScan(args[1:])
	case "init":
		return cmdInit(args[1:])
	case "connect":
		return cmdConnect(args[1:])
	case "open":
		return cmdOpen(args[1:])
	case dispatchApps:
		// `gregale apps ls` is an alias for the default list action.
		if len(args) > 1 && args[1] == "ls" {
			return cmdApps()
		}
		// `gregale apps -q <slug>` is the delete path.
		if len(args) > 1 && (args[1] == "-q" || args[1] == "--quiet") {
			return cmdAppsRm(args[2:])
		}
		return cmdApps()
	case dispatchDeployments:
		// `gregale deployments [--limit N|--before C|--all]` — list.
		// Place before appSlugFallback so the singular never shadows it.
		return cmdDeployments(args[1:])
	case dispatchDeployment:
		// `gregale deployment <id>` — get one. Must come before appSlugFallback
		// so the singular is never misread as an app slug.
		return cmdDeployment(args[1:])
	case dispatchBuild:
		// `gregale build provenance <id>` — ADR-038 / Tier 3 / issue
		// #197 B3.10-read half. The parent dispatch is in
		// commands_builds.go::cmdBuild; future build-surface
		// subcommands (`logs`, `sbom`) land there without
		// touching this switch.
		return cmdBuild(args[1:])
	case appSlugFallback:
		// Routes to cmdAppDispatch which knows the new scale/rename
		// subcommand form and falls back to the legacy flag-form
		// (commands2.go::cmdApp) for backwards compat.
		return cmdAppDispatch(args[1:])
	case "ps":
		return cmdPS(args[1:])
	case statusLiteral:
		return cmdStatus(args[1:])
	case "env":
		return cmdEnv(args[1:])
	case "plan":
		return cmdPlan(args[1:])
	case "dashboard":
		return cmdDashboard(args[1:])
	case "rollback":
		return cmdRollback(args[1:])
	case "park":
		return cmdPark(args[1:])
	case "wake":
		return cmdWake(args[1:])
	case "domains":
		return cmdDomains(args[1:])
	case "crons":
		return cmdCrons(args[1:])
	case "keys":
		return cmdKeys(args[1:])
	case dispatchSignKeys:
		return cmdSignKeys(args[1:])
	case dispatchTrustedPublishers:
		// Issue #472 / ADR-054 — operator CLI for the per-app
		// cosign trusted-publisher list. Admin API key required;
		// every leaf calls authedClient() and hits apid. The
		// sibling operator surface `sign-keys` (above) hits the
		// local fs, never apid.
		return cmdTrustedPublishers(args[1:])
	case dispatchHostAge:
		// Operator-side host.age rotation (issue #316 / ADR-057).
		// Same operator-only surface as sign-keys / pki: every
		// leaf is a local fs operation against /etc/faas/secrets/.
		// Sibling — never reuse the `keys` namespace (that's the
		// customer API-key manager in commands2.go::cmdKeys which
		// hits apid via authedClient()).
		return cmdHostAge(args[1:])
	case dispatchBackup:
		return cmdBackup(args[1:])
	case dispatchPKI:
		// Operator-side local-dev PKI bootstrap (ADR-052). Issues
		// /etc/faas/tls/{ca,<daemon>/} material for multi-box mTLS.
		// Distinct from sign-keys because the trust root is the CA,
		// not the per-box cosign keypair.
		return cmdPKI(args[1:])
	case "secrets":
		return cmdSecrets(args[1:])
	case "account":
		return cmdAccount(args[1:])
	case "usage":
		// cmdUsage dispatches: bare `gregale usage` → per-app rows;
		// `gregale usage summary [--month X]` → account roll-up.
		// Unknown positionals are rejected by the dispatcher.
		return cmdUsage(args[1:])
	case "invoices":
		return cmdInvoices(args[1:])
	case "billing":
		// Issue #253: dashboard's "Open Stripe billing portal"
		// button has a CLI twin. Subcommands live in
		// commands_billing.go.
		return cmdBilling(args[1:])
	case "admin":
		return cmdAdmin(args[1:])
	case "logs":
		return cmdLogs(args[1:])
	case "tail":
		return cmdTail(args[1:])
	case "audit-events":
		// Wave 0 PR-C / ADR-047: customer/operator CLI for the
		// /v1/audit-events surface. Default scope = caller's own
		// account; --kind-prefix filters (stateless.advisory is
		// the Wave 0 use case); --include-anonymous surfaces the
		// rare subject=NULL defensive rows.
		return cmdAuditEvents(args[1:])
	case "metrics":
		// Move 1 PR-A: CLI twin for GET /v1/apps/{slug}/metrics.
		// Same data shape the dashboard panel renders, in the
		// terminal where the rest of the debugging happens.
		return cmdMetrics(args[1:])
	case "queue":
		return cmdQueueDispatch(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "gregale: unknown command %q\nRun 'gregale help' for usage.\n", args[0])
		return 1
	}
}
