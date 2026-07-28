// Synthetic-fixture driver for the FaasCpuStarvation alert (issue #301,
// ADR-044). Invokes `promtool test rules` against the fixture in
// testdata/cpu_starvation.test.yml and asserts the exit code is 0.
//
// What this exercises:
//   1. FaasCpuStarvation fires when vmmd_cpu_throttle_ratio{slice}
//      exceeds 0.8 for 5m on a per-plan sub-slice
//      (acceptance #3 of issue #301).
//   2. FaasCpuStarvation does NOT fire on a sub-threshold slice
//      (negative case — sanity check on the > 0.8 comparator).
//   3. The {slice=~"tenant-.*"} regex preserves per-slice labels so
//      Alertmanager groups per-plan; a regression that drops the
//      regex would surface here as the test reading the wrong shape.
//   4. The `for: 5m` debounce window suppresses bursts shorter than
//      5m.
//   5. The sibling cumulative counter
//      vmmd_cpu_throttle_seconds_total{account_id, app_id} is
//      exercised end-to-end via the same fixture, so the rollup
//      survives a regression that removes the gauge's emitter.
//
// Why this is the canonical synthetic-fixture test: TestFaasRulesSyntax
// validates the rule file's syntax via `promtool check rules`
// (parse-only); this test feeds real samples through the alert
// evaluator and asserts the alert opens at the configured threshold.
// No daemon-side state, no live vmmd instance required — pure
// rule-engine evaluation.
//
// Build tag: integration. Skipped on plain `go test` runs because
// promtool may not be installed on the dev box; CI installs it
// explicitly via the workflow step. Same convention as
// tenant_abuse_test.go and anomaly_score_test.go.

//go:build integration

package promqlrules_test

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestFaasCpuStarvationEval(t *testing.T) {
	if _, err := exec.LookPath("promtool"); err != nil {
		t.Skip("promtool not installed on PATH; the CI step at .github/workflows/ci.yml:113-135 runs the check unconditionally")
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	// testdata/cpu_starvation.test.yml walks up two dirs to find
	// deploy/ansible/roles/prometheus/files/faas.rules.yml via the
	// `rule_files:` directive; that path is resolved relative to
	// the test fixture file's directory. The Go test passes the
	// absolute path of the fixture to promtool.
	fixture := filepath.Join(repoRoot, "pkg", "promqlrules", "testdata", "cpu_starvation.test.yml")
	cmd := exec.Command("promtool", "test", "rules", fixture)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("promtool test rules failed for %s:\n%s\n%v", fixture, out, err)
	}
}
