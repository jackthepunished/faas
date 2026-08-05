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
// output into a typed ScanResult (issue #299 / ADR-055 / PR-2). ctx
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
func defaultGrypeRun(ctx context.Context, dir string) (*ScanResult, error) {
	return runGrypeImpl(ctx, "grype", dir)
}

// RunGrype is the production entry point for cmd/imaged when the
// operator pins grype to PATH (FAAS_GRYPE_BIN is empty). It is the
// same code path defaultGrypeRun uses, exported so cmd/imaged can
// hand a single function value to WithGrypeRun without a closure
// wrapper. Pinned by TestRunGrype_DelegatesToSubprocess in
// grype_test.go (run with FAAS_RUN_GRYPE_TESTS=1).
func RunGrype(ctx context.Context, dir string) (*ScanResult, error) {
	return defaultGrypeRun(ctx, dir)
}

// RunGrypeAt is the operator-pinned variant: when the ansible role
// installs grype at a non-PATH location (e.g. /opt/grype/bin/grype),
// cmd/imaged passes that path via FAAS_GRYPE_BIN and the closure
// inside makeGrypeRunner binds the binary to RunGrypeAt so the
// subprocess invocation doesn't depend on $PATH resolution.
func RunGrypeAt(ctx context.Context, bin, dir string) (*ScanResult, error) {
	return runGrypeImpl(ctx, bin, dir)
}

// runGrypeImpl is the shared body. Parameterised on the binary
// path so defaultGrypeRun (PATH lookup, "grype") and RunGrypeAt
// (operator-supplied absolute path) share the same parse +
// counting logic; the per-call dispatch is one switch.
//
// PR-2 (issue #464 / ADR-055): the return type changed from
// `map[string]int` to `*ScanResult`. The SeverityCounts field
// carries the same per-bucket count the pre-PR-2 map did; the
// full Vulnerability[] list is added in PR-3 when the per-deploy
// sink writes the typed payload. This PR preserves the
// fail-closed base-ext4 sidecar behaviour: the call site at
// base_stage.go::writeScanSidecar reads the counts off the
// new struct and writes the same sidecar JSON.
func runGrypeImpl(ctx context.Context, bin, dir string) (*ScanResult, error) {
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
	var counts ScanResult
	for _, m := range out.Matches {
		counts.bumpSeverity(normalizeGrypeSeverity(m.Vulnerability.Severity))
	}
	return &counts, nil
}

// ScanResult is the typed result of one grype run (issue #464 /
// ADR-055 / PR-2). The pre-PR-2 call sites returned
// `map[string]int`; the new struct carries the per-bucket count
// (SeverityCounts) and reserves space for the full
// Vulnerability list that PR-3's per-deploy sink writes.
//
// The package-level `Severity` constants below are the closed
// enum Grype normalises to (CRITICAL|HIGH|MEDIUM|LOW|UNKNOWN).
// The base-ext4 sidecar write at base_stage.go::writeScanSidecar
// reads SeverityCounts.Critical / .High / .Medium / .Low / .Unknown
// to build the legacy `findings map[string]int` payload — the
// pre-PR-2 sidecar JSON shape is byte-identical for a given
// input. The per-deploy surface (PR-3's deploy-complete hook
// in handler.go::runDeployScan) uses the full struct directly.
//
// Error carries the grype-runner error message on the
// scan_status='failed' path (PR-3 retry-exhausted backoff).
// Empty on success. Marshalled into deployments.scan_result
// jsonb so the dashboard's "scan failed" chip can render the
// underlying cause; not surfaced on the success path.
type ScanResult struct {
	SeverityCounts
	// Error is the grype-runner error message stamped on the
	// scan_status='failed' path (PR-3 retry-exhausted backoff).
	// Empty on success. Marshalled with omitempty so a successful
	// scan's jsonb payload doesn't carry the field.
	Error string `json:"error,omitempty"`
}

// bumpSeverity increments the per-bucket count for one
// normalised severity. A future severity (or an empty string
// from a malformed match) maps to UNKNOWN so the count still
// records an honest value rather than dropping the row.
func (s *ScanResult) bumpSeverity(severity string) {
	switch severity {
	case SeverityCritical:
		s.Critical++
	case SeverityHigh:
		s.High++
	case SeverityMedium:
		s.Medium++
	case SeverityLow:
		s.Low++
	case SeverityUnknown:
		s.Unknown++
	default:
		s.Unknown++
	}
}

// toMap projects the typed SeverityCounts back into the
// `map[string]int` shape the legacy base-ext4 scan sidecar
// expects (consumed by vmmd's bringUpScanCheck at
// pkg/fcvm/manager.go). The keys are the uppercase closed
// enum (CRITICAL/HIGH/MEDIUM/LOW/UNKNOWN). The map shape
// stays byte-identical to the pre-PR-2 output for any given
// input so the sidecar contract is preserved.
func (s *ScanResult) toMap() map[string]int {
	if s == nil {
		return nil
	}
	return map[string]int{
		SeverityCritical: s.Critical,
		SeverityHigh:     s.High,
		SeverityMedium:   s.Medium,
		SeverityLow:      s.Low,
		SeverityUnknown:  s.Unknown,
	}
}

// SeverityCounts is the per-bucket count of CVEs in Grype's
// closed vocabulary. Mirrors pkg/api.SeverityCounts (the
// customer-facing DTO) but lives in imaged to keep the
// internal type dependency-free. PR-3's sink marshals this
// struct directly into the deployments.scan_result jsonb
// column.
type SeverityCounts struct {
	Critical int
	High     int
	Medium   int
	Low      int
	Unknown  int
}

// Severity closed enum (issue #299 / ADR-055). Grype emits
// a leading uppercase letter (Critical, High, Medium, Low,
// Negligible, Unknown); the in-package constant set is
// upper-case to match the pkg/api closed enum and the
// existing vmmd scan sidecar. Negligible collapses into LOW
// (the existing normalizeGrypeSeverity convention).
const (
	SeverityCritical = "CRITICAL"
	SeverityHigh     = "HIGH"
	SeverityMedium   = "MEDIUM"
	SeverityLow      = "LOW"
	SeverityUnknown  = "UNKNOWN"
)

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
