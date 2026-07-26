// Synthetic-fixture test for the FaasErrorRateSpike and
// FaasErrorRateDrop alerts (issue #303 follow-up to PR #336 /
// ADR-039 §Open follow-ups).
//
// What this exercises:
//   1. FaasErrorRateSpike — `max(faas_apid_error_rate_ratio:by_route)
//      > 2` for 10m. The alert must fire when a single route's
//      error-rate ratio crosses 2x for the configured `for:`
//      window.
//   2. FaasErrorRateDrop — `(ratio < 0.5) and (5m > 0.001)` for
//      15m. The `> 0.001 err/s` guard prevents false positives on
//      idle routes; the test pins the guard by feeding steady
//      traffic that crosses the floor.
//   3. Negative case — steady traffic with no error-rate change
//      must NOT fire either alert.
//
// Why this is the canonical synthetic-fixture test for the follow-up
// PR scope: TestFaasRulesSyntax validates the rule file parse-only;
// TestFaasAnomalyScoreEval pins the platform-drift aggregator. This
// file pins the missing two pieces — error-rate spike / drop alerts
// at the threshold level. No daemon-side state, no live apid
// instance required — pure rule-engine evaluation.
//
// Build tag: integration. Skipped on plain `go test` runs because
// promtool may not be installed on the dev box; CI installs it
// explicitly via the workflow step at .github/workflows/ci.yml.
// The convention matches the existing TestFaasRulesSyntax and
// TestFaasAnomalyScoreEval precedents.

//go:build integration

package promqlrules_test

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestFaasErrorRateEval(t *testing.T) {
	if _, err := exec.LookPath("promtool"); err != nil {
		t.Skip("promtool not installed on PATH; the CI step at .github/workflows/ci.yml runs the check unconditionally")
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	// testdata/error_rate.test.yml walks up two dirs to find
	// deploy/ansible/roles/prometheus/files/faas.rules.yml via the
	// `rule_files:` directive; that path is resolved relative to
	// the test fixture file's directory. The Go test passes the
	// absolute path of the fixture to promtool.
	fixture := filepath.Join(repoRoot, "pkg", "promqlrules", "testdata", "error_rate.test.yml")
	cmd := exec.Command("promtool", "test", "rules", fixture)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("promtool test rules failed for %s:\n%s\n%v", fixture, out, err)
	}
}
