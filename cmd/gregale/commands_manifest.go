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
)

const dispatchManifest = "manifest"

// subValidate is the leaf name. Render / Install follow in PR-2 / PR-3.
const subValidate = "validate"

// cmdManifestDispatch is the parent dispatcher. With zero args it
// prints usage; with `validate` it fans to cmdManifestValidate.
func cmdManifestDispatch(args []string) int {
	if len(args) == 0 {
		printManifestUsage(os.Stderr)
		return 1
	}
	switch args[0] {
	case subValidate:
		return cmdManifestValidate(args[1:])
	case "--help", "-h":
		printManifestUsage(os.Stderr)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "gregale manifest: unknown subcommand %q (expected: validate)\n", args[0])
		return 1
	}
}

func printManifestUsage(w io.Writer) {
	fmt.Fprintf(w, `usage: gregale manifest <subcommand> [flags]

Subcommands:
  validate    Validate a manifest YAML file (canonical path: pkg/manifest.Validate)

Flags (validate):
  --file PATH   Path to the manifest YAML file (required).
  --json        Emit JSON output instead of human-readable text.
                FAAS_JSON=1 env also works.

Exit codes:
  0  Manifest is valid.
  1  Manifest is invalid (one or more validation errors).
  3  Manifest could not be loaded (file missing, parse error,
     or unsupported schema_version).

Examples:
  gregale manifest validate --file=deploy/manifest/examples/splitbox.example.yaml
  gregale manifest validate --file=splitbox.yaml --json
`)
}

// cmdManifestValidate runs the canonical validator on the manifest
// at --file. The function returns 0 for a valid manifest, 1 for a
// load failure, 3 for a validation failure; the codes are distinct
// from the existing gregale 0/1/2/3 convention so the CI gate
// (`make lint-manifest`) can distinguish "manifest is fine" from
// "manifest is broken" from "we can't even read the file".
func cmdManifestValidate(args []string) int {
	if len(args) > 0 && (args[0] == "--help" || args[0] == "-h") {
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
		fmt.Fprintln(os.Stderr, "gregale manifest validate: --file is required")
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
			fmt.Fprintf(os.Stderr, "gregale manifest validate: %s: %v\n", *filePath, err)
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
			fmt.Fprintf(os.Stdout, "%s: valid (schema=%s, hosts=%d, daemons=%d)\n",
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
		fmt.Fprintf(os.Stdout, "%s: invalid (%d errors)\n", *filePath, len(errs))
		for _, e := range errs {
			fmt.Fprintf(os.Stdout, "  %s: %s\n", e.Path, e.Message)
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
		fmt.Fprintf(os.Stderr, "gregale manifest: marshal: %v\n", err)
		return
	}
	fmt.Fprintln(w, string(b))
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
