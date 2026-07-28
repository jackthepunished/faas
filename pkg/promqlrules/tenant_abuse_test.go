// Synthetic-fixture driver for the FaasTenantAbuse alert (issue #300,
// ADR-041). Invokes `promtool test rules` against the fixture in
// testdata/tenant_abuse.test.yml and asserts the exit code is 0.
//
// What this exercises:
//   1. FaasTenantAbuse fires on a single customer at > 500 rps for
//      10m (acceptance #3 of issue #300).
//   2. FaasTenantAbuse does NOT fire on a sub-threshold customer.
//   3. The `account_id!="other"` matcher excludes the overflow
//      bucket from the trigger (a regression that removed the
//      matcher would trip the alert on overflow-only data).
//   4. The `topk(20, ...)` aggregator preserves per-account labels
//      so Alertmanager routes per-customer.
//   5. The `for: 10m` debounce window suppresses bursts shorter
//      than 10m.
//
// Why this is the canonical synthetic-fixture test: TestFaasRulesSyntax
// validates the rule file's syntax via `promtool check rules`
// (parse-only); this test feeds real samples through the alert
// evaluator and asserts the alert opens at the configured threshold.
// No daemon-side state, no live apid instance required — pure
// rule-engine evaluation.
//
// Build tag: integration. Skipped on plain `go test` runs because
// promtool may not be installed on the dev box; CI installs it
// explicitly via the workflow step. Same convention as
// anomaly_score_test.go and error_rate_test.go.

//go:build integration

package promqlrules_test

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestFaasTenantAbuseEval(t *testing.T) {
	if _, err := exec.LookPath("promtool"); err != nil {
		t.Skip("promtool not installed on PATH; the CI step at .github/workflows/ci.yml:113-135 runs the check unconditionally")
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	// testdata/tenant_abuse.test.yml walks up two dirs to find
	// deploy/ansible/roles/prometheus/files/faas.rules.yml via the
	// `rule_files:` directive; that path is resolved relative to the
	// test fixture file's directory. The Go test passes the absolute
	// path of the fixture to promtool.
	fixture := filepath.Join(repoRoot, "pkg", "promqlrules", "testdata", "tenant_abuse.test.yml")
	cmd := exec.Command("promtool", "test", "rules", fixture)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("promtool test rules failed for %s:\n%s\n%v", fixture, out, err)
	}
}
