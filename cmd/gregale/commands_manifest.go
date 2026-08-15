// commands_manifest.go — operator-side CLI for the split-box
// deployment manifest (issue #911 / ADR-110).
//
// `gregale manifest` is the operator surface that loads the
// deployment manifest, validates its shape, and (in later PRs)
// renders it to /etc/faas/*.toml + systemd units + cgroup
// subtree_control. PR-0 ships the validator only — the renderer
// (PR-2) and the release bundle installer (PR-3) are separate
// leaves that come later.
//
// Dispatcher shape (mirrors commands_sign_keys.go):
//
//   gregale manifest validate --file PATH [--json]
//
// Flags are flag-package's stdlib. Output is JSON when --json
// (matches the gregale-wide convention of `FAAS_JSON=1` /
// `--json`); human-readable otherwise. The validator runs the
// canonical pkg/manifest.Manifest.Validate path so any future
// reader (the renderer, the doctor, the release bundle installer)
// sees the same failures the operator sees here.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/onebox-faas/faas/pkg/manifest"
	"github.com/onebox-faas/faas/pkg/renderer"
)

// subValidate is the leaf name. Render follows in PR-2.
const (
	subValidate = "validate"
	subRender   = "render"
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
	case flagHelpShort, flagHelpLong:
		printManifestUsage(os.Stderr)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "gregale manifest: unknown subcommand %q (expected: validate, render)\n", args[0])
		return 1
	}
}

func printManifestUsage(w io.Writer) {
	_, _ = fmt.Fprintf(w, `usage: gregale manifest <subcommand> [flags]

Subcommands:
  validate    Validate a manifest YAML file (canonical path: pkg/manifest.Validate)
  render      Render a validated manifest to /etc/faas/*.toml +
              systemd units + cgroup subtree_control + PKI leaves
              (canonical path: pkg/renderer.Render)

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
  --cgroup-root DIR        cgroup v2 mount root (default /sys/fs/cgroup).
  --host-san-file PATH     Optional JSON file with per-host SANs.
  --dry-run                Compute all outputs but do not write.
  --json                   Emit JSON output instead of human-readable text.

Exit codes:
  0  Manifest is valid / render succeeded (or short-circuited).
  1  Manifest is invalid (one or more validation errors) / render
     failed (validator rejected, manifest missing, path rejected).
  3  Manifest could not be loaded (file missing, parse error,
     or unsupported schema_version).

Examples:
  gregale manifest validate --file=deploy/manifest/examples/splitbox.example.yaml
  gregale manifest validate --file=splitbox.yaml --json
  gregale manifest render --manifest-file=splitbox.yaml --host=fsn-1 --dry-run --json
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
		PrintUsage(os.Stderr, "usage: gregale manifest validate --file PATH", "manifest")
		return 0
	}
	fs := flag.NewFlagSet("manifest validate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	filePath := fs.String("file", "", "path to the manifest YAML file (required)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *filePath == "" {
		_, _ = fmt.Fprintln(os.Stderr, "gregale manifest validate: --file is required")
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
			_, _ = fmt.Fprintf(os.Stderr, "gregale manifest validate: %s: %v\n", *filePath, err)
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

// manifestReport is the JSON wire shape for `gregale manifest validate
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
		_, _ = fmt.Fprintf(os.Stderr, "gregale manifest: marshal: %v\n", err)
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
		PrintUsage(os.Stderr, "usage: gregale manifest render --manifest-file PATH [--host NAME] [--dry-run] [--json]", "manifest")
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
	cgroupRoot := fs.String("cgroup-root", "", "cgroup v2 mount root (default /sys/fs/cgroup)")
	hostSANFile := fs.String("host-san-file", "", "optional JSON file with per-host SANs")
	dryRun := fs.Bool("dry-run", false, "compute outputs but do not write")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *manifestFile == "" {
		_, _ = fmt.Fprintln(os.Stderr, "gregale manifest render: --manifest-file is required")
		return 1
	}
	opts := renderer.RenderOptions{
		ManifestPath: *manifestFile,
		Host:         *host,
		ReleasesRoot: *releasesRoot,
		EtcFaasDir:   *etcFaasDir,
		SystemdDir:   *systemdDir,
		PKIRootDir:   *pkiRootDir,
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
			_, _ = fmt.Fprintf(os.Stderr, "gregale manifest render: %v\n", err)
		}
		return 1
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
