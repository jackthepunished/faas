// Synthetic-fixture test for the per-account anomaly score recording
// rule (faas_apid_anomaly_score:by_account) and the FaasTrafficAnomaly
// 10+-accounts aggregator alert (issue #303 follow-up to PR #336 /
// ADR-039).
//
// What this exercises:
//   1. faas_apid_anomaly_score:by_account — the per-account ratio of
//      current 5m rate to 3d trailing average. PR #336 computed this
//      inline in the per-account alerts; the follow-up lifts it into
//      a recording rule so FaasTrafficAnomaly can read from a
//      precomputed series rather than recomputing on every eval.
//   2. FaasTrafficAnomaly — the count(...) >= 10 alert. It must fire
//      on a synthetic 10-account coordinated burst (acceptance #3 of
//      issue #303) and must NOT fire on a single-account burst
//      (negative-case pin).
//
// Why this is the canonical synthetic-fixture test (acceptance #5 of
// issue #303): TestFaasRulesSyntax validates the rule file's syntax
// via `promtool check rules` (parse-only); this test feeds real
// samples through the recording-rule engine and asserts the alert
// opens at the configured threshold. No daemon-side state, no live
// apid instance required — pure rule-engine evaluation.
//
// Build tag: integration. Skipped on plain `go test` runs because
// promtool may not be installed on the dev box; CI installs it
// explicitly via the workflow step. The convention matches the
// existing TestFaasRulesSyntax precedent at rules_test.go:15.

//go:build integration

package promqlrules_test

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestFaasAnomalyScoreEval(t *testing.T) {
	if _, err := exec.LookPath("promtool"); err != nil {
		t.Skip("promtool not installed on PATH; the CI step at .github/workflows/ci.yml:113-135 runs the check unconditionally")
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	// testdata/anomaly_score.test.yml walks up two dirs to find
	// deploy/ansible/roles/prometheus/files/faas.rules.yml via the
	// `rule_files:` directive; that path is resolved relative to the
	// test fixture file's directory. The Go test passes the absolute
	// path of the fixture to promtool.
	fixture := filepath.Join(repoRoot, "pkg", "promqlrules", "testdata", "anomaly_score.test.yml")
	cmd := exec.Command("promtool", "test", "rules", fixture)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("promtool test rules failed for %s:\n%s\n%v", fixture, out, err)
	}
}
