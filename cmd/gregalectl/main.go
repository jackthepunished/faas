// Command gregalectl is the operator-only companion CLI to gregale
// (issue #911 / ADR-110 PR-6.5).
//
// gregalectl ships the cluster-install surface that operators run by
// hand or wire into bootstrap/ansible:
//
//   - manifest validate|render       — split-box deployment manifest
//   - release bundle|install         — cluster-shipped release bundle
//   - host-age init|rotate|status    — operator host.age rotation
//   - pki init|status|rotate         — local-dev PKI bootstrap
//   - sign-keys init|rotate|status   — cosign sign keypair (local fs)
//   - node-key init|rotate|status    — per-node CapacityReport keypair
//   - backup init|unseal-archive-creds|unseal-rclone — backup creds
//   - trusted-publishers stays in `gregale` (admin API surface; see
//     PR-6.5 deviation note) — NOT shipped here.
//   - version, completion, man        — internal surface
//
// gregale (the customer-facing CLI) and gregalectl are separate
// binaries built from `cmd/gregale/` and `cmd/gregalectl/` respectively.
// No shared internal package at this PR — duplication is intentional
// (PR-7 may extract cmd/internal/cliutil/).
//
// Exit codes follow docs/faas_ux_spec.md §3.2: 0 ok, 1 user error, 2
// auth, 3 platform/infra.
package main

import (
	"fmt"
	"os"

	"github.com/onebox-faas/faas/pkg/wire"
)

// docsURL is the canonical link printed at the bottom of the usage
// string. Mirrors cmd/gregale/main.go:23 — operator topics land at
// docs.gregale.dev/cli/<topic> until PR-7 splits /cli/ from /operator/.
var docsURL = "https://" + wire.DocsHost

var usage = `gregalectl — operator companion CLI for cluster install + lifecycle.

Usage:
  gregalectl <command> [flags]

Commands:
  manifest     Validate/render a split-box deployment manifest (manifest validate|render; issue #911 / ADR-110)
  release      Materialise / install a cluster-shipped release bundle (release bundle|install --git-sha SHA)
  host-age     Operator host.age rotation (host-age init|rotate|status|prune-previous)
  pki          Operator local-dev PKI bootstrap (pki init|status|rotate)
  sign-keys    Provision the cosign sign keypair (sign-keys init|rotate|status; --sign-key / --verify-key)
  node-key     Provision the per-node CapacityReport signing keypair (node-key init|rotate|status)
  backup       Operator rclone / archive credentials (backup init|unseal-archive-creds|unseal-rclone)
  version      Print the CLI version
  completion   Print a shell completion script (bash|zsh|fish|powershell)
  man          Print the gregalectl(1) man page (or gregalectl-<command>(1) with one arg)
  help         Print this usage block

Run 'gregalectl <command> --help' for command details.

Global flags:
  --json         Machine-readable output on every command. Equivalent
                 env: FAAS_JSON=1. Negate with --json=false.
Docs: ` + docsURL + `
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func init() {
	// Mirror cmd/gregale/main.go:114 — wire.Version into the man-page
	// `.TH GREGALECTL(1) "version"` header at process boot.
	gregalectlVersion = wire.Version
}

func run(args []string) int {
	args = applyJSONFlag(args)
	if len(args) == 0 {
		fmt.Print(usage)
		return 0
	}
	switch args[0] {
	case "version", "--version", "-v":
		if len(args) > 1 && (args[1] == "--help" || args[1] == "-h") {
			PrintUsage(os.Stderr, "usage: gregalectl version", "version")
			return 0
		}
		fmt.Printf("gregalectl %s\n", wire.Version)
		return 0
	case "help", "--help", "-h":
		fmt.Print(usage)
		return 0
	case "completion":
		// Tier A8 / ADR-083. Routes to one of bash|zsh|fish|powershell
		// via cmdCompletion; the dispatcher is in completion.go.
		return cmdCompletion(args[1:])
	case "man":
		// Tier A8 / ADR-083. No arg → gregalectl(1); one arg →
		// gregalectl-<command>(1). Dispatcher is in man.go.
		return cmdMan(args[1:])
	case "manifest":
		// Issue #911 / ADR-110: operator-side manifest loader.
		// `gregalectl manifest validate --file=PATH` runs the
		// canonical validator (pkg/manifest.Validate);
		// `gregalectl manifest render --manifest-file=PATH` runs
		// pkg/renderer.Render (PR-2). The dispatcher is
		// cmdManifestDispatch in commands_manifest.go.
		return cmdManifestDispatch(args[1:])
	case "release":
		// Issue #911 / ADR-110: cluster-shipped release bundle
		// (PR-3). `gregalectl release bundle` materialises the
		// daemon-binary bundle and INSERTs into release_bundles;
		// `gregalectl release install` flips the local
		// /opt/faas/current symlink + stamps applied_at. The
		// dispatcher is cmdReleaseDispatch in commands_release.go.
		return cmdReleaseDispatch(args[1:])
	case dispatchHostAge:
		// Operator-side host.age rotation (issue #316 / ADR-057).
		// Local fs only — never hits apid.
		return cmdHostAge(args[1:])
	case dispatchPKI:
		// Operator-side local-dev PKI bootstrap (ADR-052). Issues
		// /etc/faas/tls/{ca,<daemon>/} material for multi-box mTLS.
		return cmdPKI(args[1:])
	case dispatchSignKeys:
		// Operator-side cosign sign keypair. Writes
		// /etc/faas/secrets/{sign.key, sign-pub.pem}; never hits apid.
		return cmdSignKeys(args[1:])
	case dispatchNodeKey:
		// ADR-053 — operator-side provisioning for the per-node
		// CapacityReport signing keypair. Writes
		// /etc/faas/secrets/vmmd/{node.key (0400), node.pub (0444)}
		// and prints the key_id at init time.
		return cmdNodeKey(args[1:])
	case dispatchBackup:
		// Operator rclone config + log-archive credentials. cmdBackup
		// fans to init / unseal-rclone / unseal-archive-creds. Local
		// fs only — never hits apid.
		return cmdBackup(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "gregalectl: unknown command %q\nRun 'gregalectl help' for usage.\n", args[0])
		return 1
	}
}
