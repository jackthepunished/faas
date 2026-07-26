// Tests that the Prometheus rule file at
// deploy/ansible/roles/prometheus/files/faas.rules.yml is parseable
// by promtool. Issue #303 / ADR-039 introduces the first recording
// rules in the repo; this is the first Go-level gate that asserts
// the rule file is a valid Prometheus artifact. The CI step at
// .github/workflows/ci.yml:113-135 already runs the same check
// unconditionally — this test is the local-runner equivalent so devs
// can validate rule-file edits without pushing.
//
// Build tag: integration. Skipped on plain `go test` runs because
// promtool may not be installed on the dev box; CI installs it
// explicitly via the workflow step. The convention matches the
// repo's other integration-gated tests.

//go:build integration

package promqlrules_test

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestFaasRulesSyntax(t *testing.T) {
	if _, err := exec.LookPath("promtool"); err != nil {
		t.Skip("promtool not installed on PATH; the CI step at .github/workflows/ci.yml:113-135 runs the check unconditionally")
	}
	// Walk up from the test binary's working directory to the repo
	// root, then resolve the canonical rules path. The package lives
	// at pkg/promqlrules/ so two dirs up is the repo root.
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	rel := filepath.Join(repoRoot, "deploy", "ansible", "roles", "prometheus", "files", "faas.rules.yml")
	cmd := exec.Command("promtool", "check", "rules", rel)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("promtool check rules failed for %s:\n%s\n%v", rel, out, err)
	}
}
