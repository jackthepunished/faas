// Synthetic-fixture driver for the faas_data_placement group
// (ADR-098 §9.A, issue #951). Mirrors pkg/promqlrules/anomaly_score_test.go:
// the rule file itself is validated by TestFaasRulesSyntax (parse-only);
// this test feeds real histogram + counter samples through the alert
// evaluator and asserts the four alerts in the family open at the
// configured thresholds (RTT healthy info, RTT degraded page, probe
// failure-rate warn, probe slow warn; spec §12). No meterd instance
// required — pure rule-engine evaluation.
//
// Build tag: integration. Skipped on plain `go test` runs because
// promtool may not be installed on the dev box; CI installs it
// explicitly via the workflow step at .github/workflows/ci.yml.
// The convention matches the existing TestFaasRulesSyntax /
// TestFaasAnomalyScoreEval / TestFaasCpuStarvationEval pattern.

//go:build integration

package promqlrules_test

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestFaasDataPlacementEval(t *testing.T) {
	if _, err := exec.LookPath("promtool"); err != nil {
		t.Skip("promtool not installed on PATH; the CI step at .github/workflows/ci.yml runs the check unconditionally")
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	// testdata/data_placement.test.yml's `rule_files:` directive points
	// at the sibling rules file at ../data_placement.yaml — that's the
	// path resolution the file performs relative to its own directory.
	// The Go test passes the absolute path of the fixture to promtool.
	fixture := filepath.Join(repoRoot, "pkg", "promqlrules", "testdata", "data_placement.test.yml")
	cmd := exec.Command("promtool", "test", "rules", fixture)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("promtool test rules failed for %s:\n%s\n%v", fixture, out, err)
	}
}
