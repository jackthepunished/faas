package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validManifestYAML = `schema_version: "1.0.0"
fleet:
  hosts:
    - name: fsn-1
      role: control-plane
daemons:
  schedd:
    bind: tcp://0.0.0.0:7100
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

func writeSplitboxManifest(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return p
}

func TestCmdManifestValidate_Valid(t *testing.T) {
	p := writeSplitboxManifest(t, validManifestYAML)
	if code := cmdManifestValidate([]string{"--file", p}); code != 0 {
		t.Fatalf("cmdManifestValidate = %d, want 0", code)
	}
}

func TestCmdManifestValidate_MissingFile(t *testing.T) {
	if code := cmdManifestValidate([]string{"--file", "/nonexistent/path.yaml"}); code != 3 {
		t.Fatalf("cmdManifestValidate = %d, want 3", code)
	}
}

func TestCmdManifestValidate_MissingFlag(t *testing.T) {
	if code := cmdManifestValidate([]string{}); code != 1 {
		t.Fatalf("cmdManifestValidate = %d, want 1", code)
	}
}

func TestCmdManifestValidate_Invalid(t *testing.T) {
	// Two failures: missing memory controller + bad git_sha.
	bad := strings.Replace(validManifestYAML,
		`controllers: "memory,cpu,io,pids"`, `controllers: "cpu,io,pids"`, 1)
	bad = strings.Replace(bad,
		`git_sha: abc1234567890abcdef1234567890abcdef12345`,
		`git_sha: not-a-sha`, 1)
	p := writeSplitboxManifest(t, bad)
	if code := cmdManifestValidate([]string{"--file", p}); code != 1 {
		t.Fatalf("cmdManifestValidate = %d, want 1", code)
	}
}

func TestCmdManifestValidate_JSON(t *testing.T) {
	// Smoke test that the JSON output is valid JSON and carries
	// the right shape.
	p := writeSplitboxManifest(t, validManifestYAML)
	jsonOutput = true
	defer func() { jsonOutput = false }()

	// Capture stdout.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	origStdout := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = origStdout }()

	code := cmdManifestValidate([]string{"--file", p})
	w.Close()
	if code != 0 {
		t.Fatalf("cmdManifestValidate = %d, want 0", code)
	}
	raw := make([]byte, 4096)
	n, _ := r.Read(raw)
	var report manifestReport
	if err := json.Unmarshal(raw[:n], &report); err != nil {
		t.Fatalf("JSON unmarshal: %v\nraw: %s", err, raw[:n])
	}
	if !report.Valid {
		t.Errorf("Valid = false, want true")
	}
	if report.Schema != "1.0.0" {
		t.Errorf("Schema = %q, want 1.0.0", report.Schema)
	}
	if len(report.Daemons) != 9 {
		t.Errorf("Daemons = %d, want 9", len(report.Daemons))
	}
}

func TestCmdManifestDispatch_Unknown(t *testing.T) {
	if code := cmdManifestDispatch([]string{"unknown"}); code != 1 {
		t.Fatalf("cmdManifestDispatch = %d, want 1", code)
	}
}

func TestCmdManifestDispatch_NoArgs(t *testing.T) {
	if code := cmdManifestDispatch([]string{}); code != 1 {
		t.Fatalf("cmdManifestDispatch = %d, want 1", code)
	}
}

func TestCmdManifestDispatch_Help(t *testing.T) {
	if code := cmdManifestDispatch([]string{"--help"}); code != 0 {
		t.Fatalf("cmdManifestDispatch = %d, want 0", code)
	}
}
