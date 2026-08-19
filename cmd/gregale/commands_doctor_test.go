package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
)

// TestRunDoctorChecks_LoopbackBindError pins the loopback-bind
// check's failure path. Fixture writes a server.js containing the
// canonical bad bind; the check must return status=error +
// code=app_loopback_bound + the whycopy hint containing
// "127.0.0.1" so the customer sees the root cause.
func TestRunDoctorChecks_LoopbackBindError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "server.js"), []byte("app.listen(8080, '127.0.0.1');\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	rep := runDoctorChecks(dir)
	if len(rep.Checks) != 8 {
		t.Fatalf("expected 8 checks, got %d", len(rep.Checks))
	}
	var loopback *doctorCheck
	for i := range rep.Checks {
		if rep.Checks[i].Name == "loopback-bind" {
			loopback = &rep.Checks[i]
		}
	}
	if loopback == nil {
		t.Fatalf("loopback-bind check missing")
	}
	if loopback.Status != "error" {
		t.Fatalf("expected error, got %q", loopback.Status)
	}
	if loopback.Code != api.CodeAppLoopbackBound {
		t.Fatalf("expected code %q, got %q", api.CodeAppLoopbackBound, loopback.Code)
	}
	if !strings.Contains(loopback.Hint, "127.0.0.1") {
		t.Fatalf("hint must mention 127.0.0.1, got %q", loopback.Hint)
	}
	if len(loopback.Sources) == 0 {
		t.Fatalf("expected sources to list server.js")
	}
	if !strings.Contains(loopback.Sources[0], "server.js") {
		t.Fatalf("expected sources[0] to mention server.js, got %q", loopback.Sources[0])
	}
}

// TestRunDoctorChecks_CleanRepo pins the ok path: a repo with
// no bad patterns → every check ok. The shape matters: the JSON
// output must be deterministic for snapshot consumers.
func TestRunDoctorChecks_CleanRepo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "server.js"), []byte("app.listen(process.env.PORT);\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	rep := runDoctorChecks(dir)
	for _, c := range rep.Checks {
		if c.Status != "ok" {
			t.Errorf("check %s expected ok, got %q (code=%s, hint=%s)",
				c.Name, c.Status, c.Code, c.Hint)
		}
	}
}

// TestRunDoctorChecks_EnvVarMissing pins the env-required check.
// Fixture writes source that references $DATABASE_URL but no
// .gregale/env.json declares it. The check must flag it as
// status=error + code=env_var_missing.
func TestRunDoctorChecks_EnvVarMissing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte(`import os\nprint(os.environ["DATABASE_URL"])\n`), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	rep := runDoctorChecks(dir)
	var envCheck *doctorCheck
	for i := range rep.Checks {
		if rep.Checks[i].Name == "env-required" {
			envCheck = &rep.Checks[i]
		}
	}
	if envCheck == nil {
		t.Fatalf("env-required check missing")
	}
	if envCheck.Status != "error" {
		t.Fatalf("expected error, got %q", envCheck.Status)
	}
	if envCheck.Code != api.CodeEnvVarMissing {
		t.Fatalf("expected code %q, got %q", api.CodeEnvVarMissing, envCheck.Code)
	}
	if len(envCheck.Sources) == 0 || envCheck.Sources[0] != "DATABASE_URL" {
		t.Fatalf("expected Sources[0]=DATABASE_URL, got %v", envCheck.Sources)
	}
}

// TestRunDoctorChecks_StatelessOnlyDir pins the stateless-only
// check's persistence-signal detection. Fixture has a top-level
// data/ directory → check fires with code=stateless_only_violation.
func TestRunDoctorChecks_StatelessOnlyDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rep := runDoctorChecks(dir)
	var statCheck *doctorCheck
	for i := range rep.Checks {
		if rep.Checks[i].Name == "stateless-only" {
			statCheck = &rep.Checks[i]
		}
	}
	if statCheck == nil {
		t.Fatalf("stateless-only check missing")
	}
	if statCheck.Status != "error" {
		t.Fatalf("expected error, got %q", statCheck.Status)
	}
	if statCheck.Code != api.CodeStatelessOnlyViolation {
		t.Fatalf("expected code %q, got %q", api.CodeStatelessOnlyViolation, statCheck.Code)
	}
}

// TestScanSource_SkipsVendoredDirs pins the noise-reduction
// contract. Files under vendor/, node_modules/, .git/ must not
// be scanned — preflight scans the customer's tree, and
// vendored deps are large (3-30 MB) and full of false positives.
func TestScanSource_SkipsVendoredDirs(t *testing.T) {
	dir := t.TempDir()
	vendor := filepath.Join(dir, "vendor")
	if err := os.Mkdir(vendor, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Write a vendor file that would trip the regex; the test
	// asserts scanSource skips it.
	if err := os.WriteFile(filepath.Join(vendor, "lib.js"),
		[]byte("app.listen(80, '127.0.0.1');\n"), 0o644); err != nil {
		t.Fatalf("write vendor: %v", err)
	}
	out := scanSource(dir, loopbackBindRegex, 5)
	if len(out) != 0 {
		t.Fatalf("expected 0 sources (vendor must be skipped), got %v", out)
	}
}

// TestRenderDoctorHuman_HasAllChecks pins the human-renderer
// shape. The customer-facing line count must be stable so
// script consumers grep on a fixed line index per check.
func TestRenderDoctorHuman_HasAllChecks(t *testing.T) {
	dir := t.TempDir()
	rep := runDoctorChecks(dir)
	var sb strings.Builder
	renderDoctorHuman(&sb, rep)
	out := sb.String()
	for _, name := range []string{"port-bind", "loopback-bind", "arch", "env-required", "stateless-only", "runtime-oom", "dep-install", "startup-timeout"} {
		if !strings.Contains(out, name) {
			t.Errorf("render missing check %q in:\n%s", name, out)
		}
	}
	if !strings.Contains(out, "All checks passed") {
		t.Errorf("render missing 'All checks passed' trailer in:\n%s", out)
	}
}
