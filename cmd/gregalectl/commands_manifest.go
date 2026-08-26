// commands_manifest.go — operator-side CLI for the split-box
// deployment manifest (issue #911 / ADR-110).
//
// `gregalectl manifest` is the operator surface that loads the
// deployment manifest, validates its shape, and (in later PRs)
// renders it to /etc/faas/*.toml + systemd units + cgroup
// subtree_control. PR-0 ships the validator only — the renderer
// (PR-2) and the release bundle installer (PR-3) are separate
// leaves that come later.
//
// Dispatcher shape (mirrors commands_sign_keys.go):
//
//   gregalectl manifest validate --file PATH [--json]
//
// Flags are flag-package's stdlib. Output is JSON when --json
// (matches the gregale-wide convention of `FAAS_JSON=1` /
// `--json`); human-readable otherwise. The validator runs the
// canonical pkg/manifest.Manifest.Validate path so any future
// reader (the renderer, the doctor, the release bundle installer)
// sees the same failures the operator sees here.

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/onebox-faas/faas/pkg/manifest"
	"github.com/onebox-faas/faas/pkg/releaseinstall"
	"github.com/onebox-faas/faas/pkg/renderer"
)

// subValidate is the leaf name. Render follows in PR-2.
const (
	subValidate = "validate"
	subRender   = "render"
	subAnsible  = "ansible"
)

// cmdManifestDispatch is the parent dispatcher. With zero args it
// prints usage; with `validate` it fans to cmdManifestValidate;
// with `render` it fans to cmdManifestRender (PR-2).
func cmdManifestDispatch(args []string) int {
	if len(args) == 0 {
		printManifestUsage(os.Stderr)
		return 1
	}
	switch args[0] {
	case subValidate:
		return cmdManifestValidate(args[1:])
	case subRender:
		return cmdManifestRender(args[1:])
	case subAnsible:
		return cmdManifestAnsible(args[1:])
	case flagHelpShort, flagHelpLong:
		printManifestUsage(os.Stderr)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "gregalectl manifest: unknown subcommand %q (expected: validate, render, ansible)\n", args[0])
		return 1
	}
}

func printManifestUsage(w io.Writer) {
	_, _ = fmt.Fprintf(w, `usage: gregalectl manifest <subcommand> [flags]

Subcommands:
  validate    Validate a manifest YAML file (canonical path: pkg/manifest.Validate)
  render      Render a validated manifest to /etc/faas/*.toml +
              systemd units + cgroup subtree_control + PKI leaves
              (canonical path: pkg/renderer.Render)
  ansible     Generate an inventory + host_vars tree from the same
              manifest (use it as the Ansible inventory source)

Flags (validate):
  --file PATH   Path to the manifest YAML file (required).
  --json        Emit JSON output instead of human-readable text.
                FAAS_JSON=1 env also works.

Flags (render):
  --manifest-file PATH     Path to the manifest YAML file (required).
  --host NAME              Host in the manifest to render (default: first host).
  --releases-root DIR      Releases root (default /opt/faas/releases).
  --etc-faas-dir DIR       TOML root (default /etc/faas).
  --systemd-dir DIR        systemd unit tree (default /etc/systemd/system).
  --pki-root-dir DIR       PKI root (default /etc/faas/tls).
  --pki-trust-only         validate existing leaves without requiring the CA private key.
  --cgroup-root DIR        cgroup v2 mount root (default /sys/fs/cgroup).
  --host-san-file PATH     Optional JSON file with per-host SANs.
  --dry-run                Compute all outputs but do not write.
  --json                   Emit JSON output instead of human-readable text.

Flags (ansible):
  --manifest-file PATH     Path to the manifest YAML file (required).
  --output-dir DIR         Generated Ansible root (required).
  --force                  Replace differing generated files.
  --dry-run                Print planned files without writing.
  --json                   Emit JSON output instead of human-readable text.

Exit codes:
  0  Manifest is valid / render succeeded (or short-circuited).
  1  Manifest is invalid (one or more validation errors) / render
     failed (validator rejected, manifest missing, path rejected).
  3  Manifest could not be loaded (file missing, parse error,
     or unsupported schema_version).

Examples:
  gregalectl manifest validate --file=deploy/manifest/examples/splitbox.example.yaml
  gregalectl manifest validate --file=splitbox.yaml --json
  gregalectl manifest render --manifest-file=splitbox.yaml --host=fsn-1 --dry-run --json
  gregalectl manifest ansible --manifest-file=splitbox.yaml --output-dir=/tmp/faas-ansible
`)
}

// cmdManifestValidate runs the canonical validator on the manifest
// at --file. The function returns 0 for a valid manifest, 1 for a
// load failure, 3 for a validation failure; the codes are distinct
// from the existing gregale 0/1/2/3 convention so the CI gate
// (`make lint-manifest`) can distinguish "manifest is fine" from
// "manifest is broken" from "we can't even read the file".
func cmdManifestValidate(args []string) int {
	if len(args) > 0 && (args[0] == flagHelpLong || args[0] == flagHelpShort) {
		PrintUsage(os.Stderr, "usage: gregalectl manifest validate --file PATH", "manifest")
		return 0
	}
	fs := flag.NewFlagSet("manifest validate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	filePath := fs.String("file", "", "path to the manifest YAML file (required)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *filePath == "" {
		_, _ = fmt.Fprintln(os.Stderr, "gregalectl manifest validate: --file is required")
		return 1
	}
	m, err := manifest.Load(*filePath)
	if err != nil {
		// Load errors are at the wire level (file missing, parse
		// error, TOML rejected). Surface them as exit code 3 with
		// the path attached so the CI gate can post the message
		// to the PR. Code 3 is the "platform/infra" slot in the
		// gregale-wide convention (cmd/gregale/main.go:5-6) — the
		// manifest file is unreadable, which is operator-platform
		// shape, not a user input shape.
		if jsonEnabled() {
			jsonEmit(os.Stderr, manifestReport{
				File:  *filePath,
				Valid: false,
				Load:  err.Error(),
			})
		} else {
			_, _ = fmt.Fprintf(os.Stderr, "gregalectl manifest validate: %s: %v\n", *filePath, err)
		}
		return 3
	}
	errs := m.Validate()
	if errs == nil {
		if jsonEnabled() {
			jsonEmit(os.Stdout, manifestReport{
				File:    *filePath,
				Valid:   true,
				Schema:  m.SchemaVersion,
				Hosts:   len(m.Fleet.Hosts),
				Daemons: catalogHostKeys(),
			})
		} else {
			_, _ = fmt.Fprintf(os.Stdout, "%s: valid (schema=%s, hosts=%d, daemons=%d)\n",
				*filePath, m.SchemaVersion, len(m.Fleet.Hosts), len(catalogHostKeys()))
		}
		return 0
	}
	// Sort by path so the output is deterministic across runs.
	sort.Slice(errs, func(i, j int) bool { return errs[i].Path < errs[j].Path })
	if jsonEnabled() {
		jsonEmit(os.Stdout, manifestReport{
			File:   *filePath,
			Valid:  false,
			Schema: m.SchemaVersion,
			Errors: errs,
		})
	} else {
		_, _ = fmt.Fprintf(os.Stdout, "%s: invalid (%d errors)\n", *filePath, len(errs))
		for _, e := range errs {
			_, _ = fmt.Fprintf(os.Stdout, "  %s: %s\n", e.Path, e.Message)
		}
	}
	return 1
}

// manifestReport is the JSON wire shape for `gregalectl manifest validate
// --json`. Mirrors the load failure shape so a single jq pattern
// (`-r '.errors[]? | "\(.path): \(.message)"'`) handles both valid
// and invalid runs.
type manifestReport struct {
	File    string          `json:"file"`
	Valid   bool            `json:"valid"`
	Schema  string          `json:"schema_version,omitempty"`
	Hosts   int             `json:"hosts,omitempty"`
	Daemons []string        `json:"daemons,omitempty"`
	Load    string          `json:"load_error,omitempty"`
	Errors  manifest.Errors `json:"errors,omitempty"`
}

// jsonEmit marshals v to w (with a trailing newline). The marshalling
// path is the same as the gregale-wide --json convention: indented
// JSON for scalar outputs, NDJSON for slices. For this report
// (single object) we emit indented JSON.
func jsonEmit(w io.Writer, v interface{}) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "gregalectl manifest: marshal: %v\n", err)
		return
	}
	_, _ = fmt.Fprintln(w, string(b))
}

// jsonEnabled reports whether the gregale-wide --json flag is in
// effect. The flag is set by run() before dispatch (see
// cmd/gregale/json_flag.go:jsonOutput); the manifest validator
// honors it so the CI gate (`make lint-manifest`) can parse
// failures with jq.
func jsonEnabled() bool {
	return jsonOutput
}

// catalogHostKeys returns the daemon names from the manifest
// schema's HostKeys catalog. The doctor (PR-4) consumes the same
// helper to assert the renderer (PR-2) can't drift from the
// validator.
func catalogHostKeys() []string {
	return manifest.SortedHostKeys()
}

// cmdManifestRender runs the renderer against a manifest. The
// function returns 0 for a successful render (including a no-op
// idempotent short-circuit), 1 for a load / validate / path
// failure, 3 for a renderer-level error (PKI generation failed,
// cgroup write failed, manifest.toml placement rejected, etc.).
func cmdManifestRender(args []string) int {
	if len(args) > 0 && (args[0] == flagHelpLong || args[0] == flagHelpShort) {
		PrintUsage(os.Stderr, "usage: gregalectl manifest render --manifest-file PATH [--host NAME] [--dry-run] [--json]", "manifest")
		return 0
	}
	fs := flag.NewFlagSet("manifest render", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	manifestFile := fs.String("manifest-file", "", "path to the manifest YAML file (required)")
	host := fs.String("host", "", "host in the manifest to render (default: first host)")
	releasesRoot := fs.String("releases-root", "", "releases root (default /opt/faas/releases)")
	etcFaasDir := fs.String("etc-faas-dir", "", "TOML root (default /etc/faas)")
	systemdDir := fs.String("systemd-dir", "", "systemd unit tree (default /etc/systemd/system)")
	pkiRootDir := fs.String("pki-root-dir", "", "PKI root (default /etc/faas/tls)")
	pkiTrustOnly := fs.Bool("pki-trust-only", false, "validate existing PKI leaves without requiring the CA private key")
	cgroupRoot := fs.String("cgroup-root", "", "cgroup v2 mount root (default /sys/fs/cgroup)")
	hostSANFile := fs.String("host-san-file", "", "optional JSON file with per-host SANs")
	dryRun := fs.Bool("dry-run", false, "compute outputs but do not write")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *manifestFile == "" {
		_, _ = fmt.Fprintln(os.Stderr, "gregalectl manifest render: --manifest-file is required")
		return 1
	}
	opts := renderer.RenderOptions{
		ManifestPath: *manifestFile,
		Host:         *host,
		ReleasesRoot: *releasesRoot,
		EtcFaasDir:   *etcFaasDir,
		SystemdDir:   *systemdDir,
		PKIRootDir:   *pkiRootDir,
		PKITrustOnly: *pkiTrustOnly,
		CgroupRoot:   *cgroupRoot,
		HostSANFile:  *hostSANFile,
		DryRun:       *dryRun,
	}
	report, err := renderer.Render(opts)
	if err != nil {
		// Distinguish render-side errors (validator reject, PKI
		// failure, cgroup write) from load errors. Load errors
		// already prefix "renderer: load ..."; render errors
		// prefix "renderer: <daemon>: ..." or "renderer: cgroup:
		// ...". Both surface exit code 1 (operator-visible
		// failure); code 3 is reserved for the validator's load
		// contract.
		if jsonEnabled() {
			jsonEmit(os.Stderr, struct {
				File  string `json:"file"`
				Host  string `json:"host,omitempty"`
				Error string `json:"error"`
			}{*manifestFile, *host, err.Error()})
		} else {
			_, _ = fmt.Fprintf(os.Stderr, "gregalectl manifest render: %v\n", err)
		}
		return 1
	}
	// PR-2 (issue #911 / ADR-110): persist the host's role onto
	// compute_nodes.role after the file-write phase succeeds.
	// The renderer's scope is purely filesystem; the DB write
	// belongs at the CLI leaf (the same place cmdReleaseInstall
	// writes UpsertComputeNode from PR-6). Dry-run skips the
	// DB write (the operator is browsing, not committing); an
	// empty host name (single-box dev) skips the write because
	// compute_nodes.role stays NULL.
	//
	// A missing database DSN is logged as a warning, not an error —
	// file-write succeeded, the role-write is a downstream signal
	// the doctor will detect on the next run.
	if !*dryRun && report.Host != "" && report.Role != "" {
		if err := writeComputeNodeRole(report.Host, report.Role); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "gregalectl manifest render: warning: write compute_nodes.role (host=%s, role=%s): %v\n", report.Host, report.Role, err)
		}
	}
	if jsonEnabled() {
		jsonEmit(os.Stdout, report)
	} else {
		action := "wrote"
		if report.Skipped {
			action = "skipped (idempotent)"
		}
		_, _ = fmt.Fprintf(os.Stdout, "%s: %s (host=%s, manifest_hash=%s, outputs=%d)\n",
			*manifestFile, action, report.Host, report.ManifestHash, len(report.Outputs))
		for _, o := range report.Outputs {
			_, _ = fmt.Fprintf(os.Stdout, "  %s: %s (%d bytes)\n", o.Action, o.Path, o.Bytes)
		}
		for _, a := range report.Audit {
			_, _ = fmt.Fprintf(os.Stdout, "  audit: %s\n", a)
		}
	}
	return 0
}

// writeComputeNodeRole persists the host's role onto
// compute_nodes.role after a successful render. The DB pool is
// short-lived (one open per call) — the manifest-render workflow
// is operator-triggered, not load-bearing on a long-lived pool,
// and the existing openPgPoolFromEnv helper (commands_release.go)
// is the canonical voice for the operator database DSN. A missing DSN is a soft
// warning (the render itself succeeded; the operator can re-run
// with FAAS_PG_DSN or DATABASE_URL set after the next deploy).
func writeComputeNodeRole(name, role string) error {
	pool, err := openPgPoolFromEnv(context.Background())
	if err != nil {
		return err
	}
	defer pool.Close()
	store := releaseinstall.NewStore(pool)
	updated, err := store.SetComputeNodeRole(context.Background(), name, role)
	if err != nil {
		return err
	}
	if updated {
		_, _ = fmt.Fprintf(os.Stdout, "gregalectl manifest render: compute_nodes.role updated (host=%s, role=%s)\n", name, role)
	}
	return nil
}
