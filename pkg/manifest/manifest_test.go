package manifest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validManifest = `schema_version: "1.0.0"
fleet:
  hosts:
    - name: fsn-1
      role: control-plane
      address: 10.42.0.1:7100
    - name: fsn-2
      role: compute-only
      address: 10.42.0.2:50051
daemons:
  schedd:
    bind: tcp://0.0.0.0:7100
    tls:
      cert_path: /etc/faas/tls/schedd/server.crt
      key_path: /etc/faas/tls/schedd/server.key
      ca_path: /etc/faas/tls/ca/ca.crt
      mode: "0400"
  vmmd:
    bind: tcp://0.0.0.0:50051
    tls:
      cert_path: /etc/faas/tls/vmmd/server.crt
      key_path: /etc/faas/tls/vmmd/server.key
      ca_path: /etc/faas/tls/ca/ca.crt
      mode: "0400"
overlay:
  provider: wireguard
  cidr: 10.42.0.0/24
dns:
  apps_domain: apps.gregale.dev
  mode: cloudflare
postgresql:
  dsn: postgres://faas@127.0.0.1:5432/faas
  database: faas
  migration_max_slot: 265
  policy: on-boot
release:
  id: v1.4.0-12-gabc1234
  git_sha: abc1234567890abcdef1234567890abcdef12345
  architecture: x86_64
  firecracker_version: 1.10.0
  firecracker_digest: 0000000000000000000000000000000000000000000000000000000000000000
  kernel_digest: 1111111111111111111111111111111111111111111111111111111111111111
  builder_base_digest: 2222222222222222222222222222222222222222222222222222222222222222
  runtime_base_digest: 3333333333333333333333333333333333333333333333333333333333333333
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
  ca_fingerprint: 3333333333333333333333333333333333333333333333333333333333333333
  allowed_sans:
    - schedd.faas
    - vmmd.faas
`

func TestParse_Valid(t *testing.T) {
	m, err := Parse([]byte(validManifest))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %q, want %q", m.SchemaVersion, SchemaVersion)
	}
	if len(m.Fleet.Hosts) != 2 {
		t.Errorf("len(Hosts) = %d, want 2", len(m.Fleet.Hosts))
	}
}

func TestValidate_Valid(t *testing.T) {
	m, err := Parse([]byte(validManifest))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if errs := m.Validate(); errs != nil {
		t.Fatalf("Validate: %v", errs)
	}
}

func TestValidate_EmptyFile(t *testing.T) {
	_, err := Parse([]byte(""))
	if err == nil {
		t.Fatal("Parse(\"\") = nil, want error")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("Parse error %q, want \"empty\"", err)
	}
}

func TestValidate_UnsupportedVersion(t *testing.T) {
	s := strings.Replace(validManifest, `schema_version: "1.0.0"`,
		`schema_version: "9.9.9"`, 1)
	m, err := Parse([]byte(s))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	errs := m.Validate()
	if errs == nil {
		t.Fatal("Validate = nil, want errors")
	}
	if !errors.Is(errs, ErrInvalid) {
		t.Errorf("Validate errors = %v, want errors.Is(_, ErrInvalid)", errs)
	}
	if !strings.Contains(errs.Error(), "unsupported version") {
		t.Errorf("Validate error %q, want \"unsupported version\"", errs)
	}
}

func TestValidate_DuplicateHost(t *testing.T) {
	s := strings.Replace(validManifest,
		`- name: fsn-1
      role: control-plane
      address: 10.42.0.1:7100
    - name: fsn-2
      role: compute-only
      address: 10.42.0.2:50051`,
		`- name: fsn-1
      role: control-plane
      address: 10.42.0.1:7100
    - name: fsn-1
      role: compute-only
      address: 10.42.0.2:50051`, 1)
	errs := mustParseValidate(t, s)
	if errs == nil {
		t.Fatal("Validate = nil, want duplicate-host error")
	}
	if !strings.Contains(errs.Error(), "duplicate host") {
		t.Errorf("Validate error %q, want \"duplicate host\"", errs)
	}
}

func TestValidate_UnknownRole(t *testing.T) {
	s := strings.Replace(validManifest, `role: control-plane`,
		`role: compute-onry`, 1)
	errs := mustParseValidate(t, s)
	if errs == nil {
		t.Fatal("Validate = nil, want unknown-role error")
	}
	if !strings.Contains(errs.Error(), "unknown role") {
		t.Errorf("Validate error %q, want \"unknown role\"", errs)
	}
}

func TestValidate_MissingMemoryController(t *testing.T) {
	s := strings.Replace(validManifest, `"memory,cpu,io,pids"`, `"cpu,io,pids"`, 1)
	errs := mustParseValidate(t, s)
	if errs == nil {
		t.Fatal("Validate = nil, want missing-memory error")
	}
	if !strings.Contains(errs.Error(), `"memory"`) {
		t.Errorf("Validate error %q, want memory controller message", errs)
	}
}

func TestValidate_BadCIDR(t *testing.T) {
	s := strings.Replace(validManifest, `cidr: 10.42.0.0/24`, `cidr: not-a-cidr`, 1)
	errs := mustParseValidate(t, s)
	if errs == nil {
		t.Fatal("Validate = nil, want bad-cidr error")
	}
	if !strings.Contains(errs.Error(), "cidr") {
		t.Errorf("Validate error %q, want cidr message", errs)
	}
}

func TestValidate_BadHostAddress(t *testing.T) {
	// A single token with no path qualifier and no colon is not a
	// host:port; the validator rejects it.
	s := strings.Replace(validManifest, `address: 10.42.0.1:7100`,
		`address: not-a-host`, 1)
	errs := mustParseValidate(t, s)
	if errs == nil {
		t.Fatal("Validate = nil, want bad-address error")
	}
	if !strings.Contains(errs.Error(), "must be host:port") {
		t.Errorf("Validate error %q, want host:port message", errs)
	}
}

func TestValidate_AppsDomain(t *testing.T) {
	s := strings.Replace(validManifest, `apps_domain: apps.gregale.dev`,
		`apps_domain: -not-a-domain`, 1)
	errs := mustParseValidate(t, s)
	if errs == nil {
		t.Fatal("Validate = nil, want apps_domain error")
	}
	if !strings.Contains(errs.Error(), "apps_domain") {
		t.Errorf("Validate error %q, want apps_domain message", errs)
	}
}

func TestValidate_BadOctalMode(t *testing.T) {
	s := strings.Replace(validManifest, `mode: "0400"`, `mode: "not-octal"`, 1)
	errs := mustParseValidate(t, s)
	if errs == nil {
		t.Fatal("Validate = nil, want bad-mode error")
	}
	if !strings.Contains(errs.Error(), "octal") {
		t.Errorf("Validate error %q, want octal message", errs)
	}
}

func TestValidate_BadGitSHA(t *testing.T) {
	s := strings.Replace(validManifest,
		`git_sha: abc1234567890abcdef1234567890abcdef12345`,
		`git_sha: not-a-sha`, 1)
	errs := mustParseValidate(t, s)
	if errs == nil {
		t.Fatal("Validate = nil, want bad-git-sha error")
	}
	if !strings.Contains(errs.Error(), "git_sha") {
		t.Errorf("Validate error %q, want git_sha message", errs)
	}
}

func TestValidate_StoragePathAbsolute(t *testing.T) {
	s := strings.Replace(validManifest, `fast_root: /srv/fc`, `fast_root: relative/path`, 1)
	errs := mustParseValidate(t, s)
	if errs == nil {
		t.Fatal("Validate = nil, want relative-path error")
	}
	if !strings.Contains(errs.Error(), "absolute path") {
		t.Errorf("Validate error %q, want absolute-path message", errs)
	}
}

// TestValidateTOMLPlacement_DuplicateTLSUnderComputeNode covers the
// load-bearing bug class from issue #911: keys belonging to the
// top-level cluster re-declared under [compute_node]. The post-fix
// invariant is that the renderer refuses to emit these keys.
func TestValidateTOMLPlacement_DuplicateTLSUnderComputeNode(t *testing.T) {
	rendered := map[string]string{
		"socket_path":                "/run/faas/vmmd.sock",
		"listen_addr":                "tcp://0.0.0.0:50051",
		"tls_cert_path":              "/etc/faas/tls/vmmd/server.crt",
		"compute_node.name":          "fsn-2",
		"compute_node.tls_cert_path": "/etc/faas/tls/vmmd/server.crt",
	}
	errs := ValidateTOMLPlacement("vmmd", rendered)
	if errs == nil {
		t.Fatal("ValidateTOMLPlacement = nil, want tombstone error")
	}
	if !strings.Contains(errs.Error(), "tombstone") {
		t.Errorf("ValidateTOMLPlacement error %q, want tombstone message", errs)
	}
}

func TestValidateTOMLPlacement_ScheddClientInComputeNode(t *testing.T) {
	rendered := map[string]string{
		"socket_path":                          "/run/faas/vmmd.sock",
		"compute_node.schedd_client_cert_path": "/etc/faas/tls/schedd/server.crt",
	}
	errs := ValidateTOMLPlacement("vmmd", rendered)
	if errs == nil {
		t.Fatal("ValidateTOMLPlacement = nil, want tombstone error")
	}
	if !strings.Contains(errs.Error(), "schedd_client_cert_path") {
		t.Errorf("ValidateTOMLPlacement error %q, want schedd_client_cert_path", errs)
	}
}

func TestValidateTOMLPlacement_PrivateKeyAtTopLevelOK(t *testing.T) {
	// The canonical vmmd render: private keys at top level, [compute_node]
	// keys (the self-registration set) inside the section. No
	// tombstones; nothing wrong.
	rendered := map[string]string{
		"socket_path":             "/run/faas/vmmd.sock",
		"listen_addr":             "tcp://0.0.0.0:50051",
		"tls_cert_path":           "/etc/faas/tls/vmmd/server.crt",
		"tls_key_path":            "/etc/faas/tls/vmmd/server.key",
		"tls_ca_path":             "/etc/faas/tls/ca/ca.crt",
		"compute_node.name":       "fsn-2",
		"compute_node.target_url": "tcp://fsn-1:7100",
		"compute_node.vpcpus":     "160",
	}
	if errs := ValidateTOMLPlacement("vmmd", rendered); errs != nil {
		t.Fatalf("ValidateTOMLPlacement = %v, want nil", errs)
	}
}

func TestValidateTOMLPlacement_UnknownDaemon(t *testing.T) {
	errs := ValidateTOMLPlacement("totally-fake-daemon", map[string]string{"k": "v"})
	if errs == nil {
		t.Fatal("ValidateTOMLPlacement = nil, want unknown-daemon error")
	}
	if !strings.Contains(errs.Error(), "no HostKeys descriptor") {
		t.Errorf("error %q, want no-HOSTKEYS-descriptor", errs)
	}
}

func TestSortedHostKeys_Exhaustive(t *testing.T) {
	keys := SortedHostKeys()
	if len(keys) != 9 {
		t.Errorf("SortedHostKeys() = %d daemons, want 9 (manifest schema's daemons: map)", len(keys))
	}
	// Every HostKeys entry must appear in the manifest schema's
	// daemons.go map — that's the source of truth for "which
	// daemons are in the schema?" The HostKeys catalog drifts if
	// either side is updated without the other.
	for _, k := range keys {
		if !daemonInSchema(k) {
			t.Errorf("HostKeys[%q] missing from manifest schema's daemons: map", k)
		}
	}
}

func TestValidate_Exhaustive(t *testing.T) {
	// Multiple failures at once: bad version + bad CIDR + missing
	// memory controller + bad git_sha. The validator must surface
	// ALL of them (issue #911's doctor pattern — no
	// short-circuit).
	s := validManifest
	s = strings.Replace(s, `schema_version: "1.0.0"`, `schema_version: "9.9.9"`, 1)
	s = strings.Replace(s, `cidr: 10.42.0.0/24`, `cidr: not-a-cidr`, 1)
	s = strings.Replace(s, `controllers: "memory,cpu,io,pids"`, `controllers: "cpu,io,pids"`, 1)
	s = strings.Replace(s,
		`git_sha: abc1234567890abcdef1234567890abcdef12345`,
		`git_sha: not-a-sha`, 1)
	errs := mustParseValidate(t, s)
	if errs == nil {
		t.Fatal("Validate = nil, want multiple errors")
	}
	// All four failures should be present.
	for _, want := range []string{"unsupported version", "cidr", "memory", "git_sha"} {
		if !strings.Contains(errs.Error(), want) {
			t.Errorf("Validate error %q missing %q", errs, want)
		}
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	dir := t.TempDir()
	_, err := Load(filepath.Join(dir, "missing.yaml"))
	if err == nil {
		t.Fatal("Load(missing) = nil, want error")
	}
}

func TestLoad_TOMLRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "splitbox.toml")
	if err := os.WriteFile(p, []byte("schema_version = \"1.0.0\""), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := Load(p)
	if err == nil {
		t.Fatal("Load(toml) = nil, want error")
	}
	if !strings.Contains(err.Error(), "TOML") {
		t.Errorf("error %q, want TOML message", err)
	}
}

func TestLoad_Valid(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "splitbox.yaml")
	if err := os.WriteFile(p, []byte(validManifest), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	m, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if errs := m.Validate(); errs != nil {
		t.Fatalf("Validate: %v", errs)
	}
}

// mustParseValidate is a tiny helper that parses + validates and
// returns a single Errors (or nil). Keeps the table-shaped tests
// below readable.
func mustParseValidate(t *testing.T, s string) Errors {
	t.Helper()
	m, err := Parse([]byte(s))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return m.Validate()
}

// daemonInSchema is the cross-check against the manifest schema's
// `daemons:` map. The map is closed and tested via the Validate
// path; the HostKeys catalog must stay in lockstep with it. Update
// both at the same time.
func daemonInSchema(name string) bool {
	switch name {
	case "schedd", "vmmd", "apid", "meterd", "githubd",
		"gatewayd_public", "gatewayd_internal", "imaged", "builderd":
		return true
	}
	return false
}
