package imaged

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
)

// grype.go — Grype subprocess runner (issue #299).
//
// The default Grype runner shells out to the grype CLI, parses the
// JSON output, and returns per-severity finding counts as
// map[string]int. The runner is fail-soft at the parse layer
// (returns (nil, err) when the JSON is malformed), and the sidecar
// write site (pkg/imaged/base_stage.go) treats a nil-map return as
// a CRITICAL=9999 placeholder so vmmd refuses to boot any un-scanned
// base ext4 — fail-closed by design.
//
// Grype's JSON schema is the public one documented at
// https://github.com/anchore/grype (output format "json" emits a
// top-level `matches` array; each match carries
// `vulnerability.severity` ∈ {Negligible, Low, Medium, High,
// Critical, Unknown}). We lowercase the Grype severity to match
// the counter convention used elsewhere in the repo
// (CRITICAL/HIGH/MEDIUM/LOW/UNKNOWN, uppercase).

// grypeMatch is the slim subset of the Grype JSON output we
// consume. The full schema is documented at
// https://github.com/anchore/grype/blob/main/schema/json/schema-9.0.json
// but we only need vulnerability.severity per match to count
// findings. Future versions of Grype that drop this field will
// surface as a parse error here — handled by the fail-closed
// sidecar write (CRITICAL=9999 placeholder) at the call site.
type grypeMatch struct {
	Vulnerability struct {
		Severity string `json:"severity"`
	} `json:"vulnerability"`
}

// grypeOutput is the top-level shape of `grype dir:<dir> -o json`.
// The `matches` array carries one entry per detected vulnerability;
// descriptor fields (ignored here) include the source artifact and
// Grype's database version.
type grypeOutput struct {
	Matches []grypeMatch `json:"matches"`
}

// defaultGrypeRun shells out to the grype CLI and parses the JSON
// output into per-severity finding counts (issue #299). ctx
// cancellation propagates to the subprocess via exec.CommandContext
// (same pattern as pkg/fcvm/metrics.go:349's lvs invocation).
// Returns (nil, err) on a subprocess error or parse failure —
// the caller (EnsureBaseExt4's sidecar write) treats both as a
// scan-failed path and writes the fail-closed CRITICAL=9999
// placeholder.
//
// The binary is resolved via exec.LookPath (no absolute path
// override); the imaged ansible role is expected to install grype
// at a default-PATH location. A missing grype binary surfaces
// here as a `exec: "grype": executable file not found in $PATH`
// error from the subprocess — same fail-closed path.
func defaultGrypeRun(ctx context.Context, dir string) (map[string]int, error) {
	return runGrypeImpl(ctx, "grype", dir)
}

// RunGrype is the production entry point for cmd/imaged when the
// operator pins grype to PATH (FAAS_GRYPE_BIN is empty). It is the
// same code path defaultGrypeRun uses, exported so cmd/imaged can
// hand a single function value to WithGrypeRun without a closure
// wrapper. Pinned by TestRunGrype_DelegatesToSubprocess in
// grype_test.go (run with FAAS_RUN_GRYPE_TESTS=1).
func RunGrype(ctx context.Context, dir string) (map[string]int, error) {
	return defaultGrypeRun(ctx, dir)
}

// RunGrypeAt is the operator-pinned variant: when the ansible role
// installs grype at a non-PATH location (e.g. /opt/grype/bin/grype),
// cmd/imaged passes that path via FAAS_GRYPE_BIN and the closure
// inside makeGrypeRunner binds the binary to RunGrypeAt so the
// subprocess invocation doesn't depend on $PATH resolution.
func RunGrypeAt(ctx context.Context, bin, dir string) (map[string]int, error) {
	return runGrypeImpl(ctx, bin, dir)
}

// runGrypeImpl is the shared body. Parameterised on the binary
// path so defaultGrypeRun (PATH lookup, "grype") and RunGrypeAt
// (operator-supplied absolute path) share the same parse +
// counting logic; the per-call dispatch is one switch.
func runGrypeImpl(ctx context.Context, bin, dir string) (map[string]int, error) {
	cmd := exec.CommandContext(ctx, bin, "dir:"+dir, "-o", "json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("imaged: grype scan dir %q: %w (stderr=%q)", dir, err, stderr.String())
	}
	var out grypeOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("imaged: grype scan dir %q: parse json: %w", dir, err)
	}
	counts := map[string]int{}
	for _, m := range out.Matches {
		counts[normalizeGrypeSeverity(m.Vulnerability.Severity)]++
	}
	return counts, nil
}

// normalizeGrypeSeverity upper-cases Grype's severity strings to
// match the canonical closed set used by vmmd's scan sidecar
// (CRITICAL/HIGH/MEDIUM/LOW/UNKNOWN). Grype emits a leading
// uppercase letter (Critical, High, Medium, Low, Negligible,
// Unknown); the public Grype docs document this vocabulary. A
// future severity (or an empty string from a malformed match)
// collapses to UNKNOWN so the counter still records an honest
// count rather than dropping the row.
func normalizeGrypeSeverity(s string) string {
	switch s {
	case "Critical":
		return "CRITICAL"
	case "High":
		return "HIGH"
	case "Medium":
		return "MEDIUM"
	case "Low":
		return "LOW"
	case "Negligible":
		// Grype's "Negligible" severity is mapped to LOW
		// here so the dashboard's LOW row absorbs it; the
		// §12 dashboard panels do not separately chart
		// Negligible. The two-row collapse is documented at
		// memory note `grype-severity-mapping`.
		return "LOW"
	case "Unknown":
		return "UNKNOWN"
	default:
		return "UNKNOWN"
	}
}
