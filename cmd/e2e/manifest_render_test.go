// manifest_render_test.go — non-metal CI-safe acceptance for the
// `gregale manifest render` flow (PR-2 / ADR-110).
//
// This test is the e2e surface for PR-2. It exercises:
//
//   - `gregale manifest render --manifest-file ...` against a
//     fixture manifest, emitting per-daemon TOML, systemd units,
//     the faas-cp.slice unit, and cgroup subtree_control.
//   - A second render is a no-op (the SHA256 idempotent short-
//     circuit reports Skipped=true with every output
//     Action=unchanged).
//   - A bad host surfaces a non-zero exit.
//   - A missing-memory-c manifest is rejected by the validator
//     BEFORE any filesystem write.
//
// The test does not require Postgres. It does require a built
// gregale binary, which buildGregale() produces.

package e2e_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// renderManifestYAML is the minimal valid manifest used by the
// render e2e test. It is intentionally short — PR-2's contract is
// that the renderer produces /etc/faas + systemd + cgroup, not
// that the manifest has every daemon block.
const renderManifestYAML = `schema_version: "1.0.0"
fleet:
  hosts:
    - name: fsn-1
      role: single-box
daemons:
  schedd:
    bind: tcp://0.0.0.0:7100
  vmmd:
    bind: tcp://0.0.0.0:50051
overlay:
  provider: wireguard
  cidr: 10.42.0.0/24
dns:
  apps_domain: apps.gregale.dev
  mode: cloudflare
postgresql:
  dsn: postgres://faas@127.0.0.1:5432/faas
  database: faas
  migration_max_slot: 10
  policy: on-boot
release:
  id: v1.4.0
  git_sha: abc1234567890abcdef1234567890abcdef12345
  architecture: x86_64
  firecracker_version: 1.10.0
  firecracker_digest: 0000000000000000000000000000000000000000000000000000000000000000
  kernel_digest: 0000000000000000000000000000000000000000000000000000000000000000
  builder_base_digest: 0000000000000000000000000000000000000000000000000000000000000000
  runtime_base_digest: 0000000000000000000000000000000000000000000000000000000000000000
storage:
  fast_root: /srv/fc
  spool_root: /var/spool/faas
  log_root: /var/log/faas
  run_dir: /run/faas
cgroups:
  slice: faas-cp.slice
  controllers: "memory,cpu,io,pids"
pki:
  root_dir: /etc/faas/tls
  ca_fingerprint: 0000000000000000000000000000000000000000000000000000000000000000
`

// writeRenderManifest drops renderManifestYAML into a temp file and
// returns its path.
func writeRenderManifest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(p, []byte(renderManifestYAML), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return p
}

// TestManifestRender_DryRunRoundTrip is the primary acceptance:
// dry-run with a valid manifest exits 0 and produces a JSON
// RenderReport describing every file that would have been written.
func TestManifestRender_DryRunRoundTrip(t *testing.T) {
	manifestPath := writeRenderManifest(t)
	gregalectlBin := buildGregaleCtl(t)

	cmd := exec.Command(gregalectlBin,
		"manifest", "render",
		"--manifest-file", manifestPath,
		"--dry-run",
		"--json",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("dry-run exit=%d stderr=%q", ee.ExitCode(), stderr.String())
		}
		t.Fatalf("dry-run exec: %v", err)
	}

	var report struct {
		Host         string `json:"host"`
		ManifestHash string `json:"manifest_hash"`
		Outputs      []struct {
			Path   string `json:"path"`
			Bytes  int    `json:"bytes"`
			SHA256 string `json:"sha256"`
			Action string `json:"action"`
		} `json:"outputs"`
		Skipped bool `json:"skipped"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode --json: %v\nstdout: %s", err, stdout.String())
	}
	if report.Host != "fsn-1" {
		t.Errorf("Host = %q, want fsn-1", report.Host)
	}
	if len(report.ManifestHash) != 64 {
		t.Errorf("ManifestHash len = %d, want 64", len(report.ManifestHash))
	}
	// At minimum the dry-run should report one entry per daemon
	// on a single-box host (8 daemons) plus the faas-cp.slice
	// unit. We don't pin the exact count to keep the test robust
	// against future daemon additions.
	if len(report.Outputs) < 9 {
		t.Errorf("Outputs len = %d, want >= 9", len(report.Outputs))
	}
	// Every output should be flagged "skipped" in dry-run mode.
	for _, o := range report.Outputs {
		if o.Action != "skipped" {
			t.Errorf("dry-run output %s.Action = %q, want skipped", o.Path, o.Action)
		}
	}
}

// TestManifestRender_WriteAndIdempotent drives the full path: first
// run writes real files; second run reports Skipped=true with
// every output Action=unchanged. This is the load-bearing gate for
// the issue #911 declarative deployment property — re-running
// `gregale manifest render` must be a no-op when nothing changed.
func TestManifestRender_WriteAndIdempotent(t *testing.T) {
	manifestPath := writeRenderManifest(t)
	gregalectlBin := buildGregaleCtl(t)
	dir := t.TempDir()

	args := []string{
		"manifest", "render",
		"--manifest-file", manifestPath,
		"--releases-root", filepath.Join(dir, "releases"),
		"--etc-faas-dir", filepath.Join(dir, "etc"),
		"--systemd-dir", filepath.Join(dir, "systemd"),
		"--pki-root-dir", filepath.Join(dir, "tls"),
		"--cgroup-root", filepath.Join(dir, "cgroup"),
	}
	run := func(extraArgs ...string) (int, string, string) {
		fullArgs := append(append([]string{}, args...), extraArgs...)
		cmd := exec.Command(gregalectlBin, fullArgs...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		code := 0
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			} else {
				t.Fatalf("exec: %v", err)
			}
		}
		return code, stdout.String(), stderr.String()
	}

	// 1. First run: must write.
	if code, _, stderr := run("--json"); code != 0 {
		t.Fatalf("first render: exit=%d stderr=%q", code, stderr)
	}
	// Spot-check the per-daemon TOML + slice unit landed.
	for _, want := range []string{
		filepath.Join(dir, "etc", "schedd.toml"),
		filepath.Join(dir, "etc", "vmmd.toml"),
		filepath.Join(dir, "systemd", "faas-schedd.service"),
		filepath.Join(dir, "systemd", "faas-cp.slice"),
		filepath.Join(dir, "cgroup", "faas.slice", "faas-cp.slice", "cgroup.subtree_control"),
	} {
		if _, err := os.Stat(want); err != nil {
			t.Errorf("first render: missing output %s: %v", want, err)
		}
	}

	// 2. Second run: must report Skipped=true + every Action=unchanged.
	code, stdout, stderr := run("--json")
	if code != 0 {
		t.Fatalf("second render: exit=%d stderr=%q", code, stderr)
	}
	var report struct {
		Skipped bool `json:"skipped"`
		Outputs []struct {
			Action string `json:"action"`
		} `json:"outputs"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("decode --json: %v\nstdout: %s", err, stdout)
	}
	if !report.Skipped {
		t.Errorf("second run Skipped = false, want true (idempotent)")
	}
	for _, o := range report.Outputs {
		if o.Action != "unchanged" {
			t.Errorf("output Action = %q, want unchanged", o.Action)
		}
	}
}

// TestManifestRender_RejectsBadHost pins that the renderer's
// host-resolution error surfaces as a non-zero exit (NOT 0).
// A silent success here would mean a typo'd --host accidentally
// re-rendered the wrong box.
func TestManifestRender_RejectsBadHost(t *testing.T) {
	manifestPath := writeRenderManifest(t)
	gregalectlBin := buildGregaleCtl(t)

	cmd := exec.Command(gregalectlBin,
		"manifest", "render",
		"--manifest-file", manifestPath,
		"--host", "no-such-host",
		"--dry-run",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		t.Fatalf("render(bad host) = nil err, want non-zero exit; stdout=%q", stdout.String())
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("render(bad host) err = %v, want ExitError", err)
	}
	if ee.ExitCode() == 0 {
		t.Errorf("render(bad host) exit = 0, want non-zero")
	}
	if !strings.Contains(stderr.String(), "no-such-host") {
		t.Errorf("render(bad host) stderr = %q; want host name in message", stderr.String())
	}
}

// TestManifestRender_RejectsMissingMemoryC pins that the validator
// (PR-0 carve-out) runs BEFORE the renderer touches the
// filesystem. A manifest with `controllers: "cpu,io,pids"` (no
// memory) must be rejected at load-then-validate time, NOT emit a
// non-functional subtree_control.
func TestManifestRender_RejectsMissingMemoryC(t *testing.T) {
	bad := strings.Replace(renderManifestYAML,
		`controllers: "memory,cpu,io,pids"`,
		`controllers: "cpu,io,pids"`, 1)
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(manifestPath, []byte(bad), 0o644); err != nil {
		t.Fatalf("write bad: %v", err)
	}

	gregalectlBin := buildGregaleCtl(t)
	dir = t.TempDir() // output target must be empty for the no-filesystem-touch pin
	cmd := exec.Command(gregalectlBin,
		"manifest", "render",
		"--manifest-file", manifestPath,
		"--releases-root", filepath.Join(dir, "releases"),
		"--etc-faas-dir", filepath.Join(dir, "etc"),
		"--systemd-dir", filepath.Join(dir, "systemd"),
		"--pki-root-dir", filepath.Join(dir, "tls"),
		"--cgroup-root", filepath.Join(dir, "cgroup"),
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		t.Fatalf("render(missing-memory) = nil err, want non-zero; stdout=%q", stdout.String())
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("render(missing-memory) err = %v, want ExitError", err)
	}
	if ee.ExitCode() == 0 {
		t.Errorf("render(missing-memory) exit = 0, want non-zero")
	}
	// The validator's missing-memory message must be in stderr.
	if !strings.Contains(stderr.String(), "memory") {
		t.Errorf("render(missing-memory) stderr = %q; want 'memory' in message", stderr.String())
	}
	// Nothing should have been written.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		// Only `manifest.yaml` may exist (we wrote it earlier
		// into a different dir, so nothing should be here).
		t.Errorf("renderer wrote %s before validator rejected manifest", e.Name())
	}
}
