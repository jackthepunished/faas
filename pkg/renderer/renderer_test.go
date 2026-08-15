package renderer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/manifest"
)

// fixtureManifest writes a minimal valid manifest YAML to a temp
// file and returns the path. The host role is `hostRole`. The
// per-daemon Daemons block is intentionally minimal — the renderer
// tolerates a missing block (Audit entry) so the test surface
// stays focused on the host-resolution / validate / short-circuit
// logic.
func fixtureManifest(t *testing.T, hostName, hostRole string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	manifestYAML := `schema_version: "1.0.0"
fleet:
  hosts:
    - name: ` + hostName + `
      role: ` + hostRole + `
daemons:
  schedd:
    bind: unix:///run/faas/schedd.sock
  apid:
    bind: unix:///run/faas/apid.sock
overlay:
  provider: tailscale
  cidr: 10.42.0.0/24
dns:
  apps_domain: apps.gregale.dev
  mode: cloudflare
postgresql:
  dsn: postgres://localhost/faas
  database: faas
  migration_max_slot: 1
  policy: on-boot
release:
  id: v1.0.0
  git_sha: 0123456789abcdef0123456789abcdef01234567
  architecture: x86_64
  firecracker_version: "1.10.0"
  firecracker_digest: ` + strings.Repeat("a", 64) + `
  kernel_digest: ` + strings.Repeat("b", 64) + `
  builder_base_digest: ` + strings.Repeat("c", 64) + `
  runtime_base_digest: ` + strings.Repeat("d", 64) + `
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
  ca_fingerprint: ` + strings.Repeat("e", 64) + `
`
	if err := os.WriteFile(path, []byte(manifestYAML), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

func TestRenderer_ValidatesManifestBeforeFilesystemTouch(t *testing.T) {
	dir := t.TempDir()
	badPath := filepath.Join(dir, "bad.yaml")
	// Bad: missing required pki.ca_fingerprint.
	badYAML := `schema_version: "1.0.0"
fleet:
  hosts:
    - name: x
      role: single-box
`
	if err := os.WriteFile(badPath, []byte(badYAML), 0o644); err != nil {
		t.Fatalf("write bad: %v", err)
	}

	_, err := Render(RenderOptions{
		ManifestPath: badPath,
		DryRun:       true,
	})
	if err == nil {
		t.Errorf("invalid manifest = nil err, want error")
	}
	// No files should have been written.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir has %d entries; expected only the manifest: %v", len(entries), names)
	}
}

func TestRenderer_ResolvesSingleBoxHost(t *testing.T) {
	dir := t.TempDir()
	path := fixtureManifest(t, "fsn-1", "single-box")
	opts := RenderOptions{
		ManifestPath: path,
		EtcFaasDir:   filepath.Join(dir, "etc"),
		SystemdDir:   filepath.Join(dir, "systemd"),
		PKIRootDir:   filepath.Join(dir, "tls"),
		CgroupRoot:   filepath.Join(dir, "cgroup"),
		DryRun:       true,
	}
	report, err := Render(opts)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if report.Host != "fsn-1" {
		t.Errorf("Host = %q, want fsn-1", report.Host)
	}
	if len(report.Outputs) == 0 {
		t.Errorf("Outputs is empty")
	}
}

func TestRenderer_ResolvesExplicitHost(t *testing.T) {
	// Write a multi-host manifest; render only the second host.
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	yaml := `schema_version: "1.0.0"
fleet:
  hosts:
    - name: fsn-1
      role: control-plane
    - name: fsn-2
      role: compute-only
daemons:
  schedd: {bind: unix:///run/faas/schedd.sock}
  vmmd: {bind: unix:///run/faas/vmmd.sock}
  apid: {bind: unix:///run/faas/apid.sock}
overlay:
  provider: tailscale
  cidr: 10.42.0.0/24
dns:
  apps_domain: apps.gregale.dev
  mode: cloudflare
postgresql:
  dsn: postgres://localhost/faas
  database: faas
  migration_max_slot: 1
  policy: on-boot
release:
  id: v1.0.0
  git_sha: 0123456789abcdef0123456789abcdef01234567
  architecture: x86_64
  firecracker_version: "1.10.0"
  firecracker_digest: ` + strings.Repeat("a", 64) + `
  kernel_digest: ` + strings.Repeat("b", 64) + `
  builder_base_digest: ` + strings.Repeat("c", 64) + `
  runtime_base_digest: ` + strings.Repeat("d", 64) + `
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
  ca_fingerprint: ` + strings.Repeat("e", 64) + `
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	opts := RenderOptions{
		ManifestPath: path,
		Host:         "fsn-2",
		EtcFaasDir:   filepath.Join(dir, "etc"),
		SystemdDir:   filepath.Join(dir, "systemd"),
		PKIRootDir:   filepath.Join(dir, "tls"),
		CgroupRoot:   filepath.Join(dir, "cgroup"),
		DryRun:       true,
	}
	report, err := Render(opts)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if report.Host != "fsn-2" {
		t.Errorf("Host = %q, want fsn-2", report.Host)
	}
}

func TestRenderer_RefusesUnknownHost(t *testing.T) {
	path := fixtureManifest(t, "fsn-1", "single-box")
	_, err := Render(RenderOptions{
		ManifestPath: path,
		Host:         "no-such-host",
		DryRun:       true,
	})
	if err == nil {
		t.Errorf("unknown host = nil err, want error")
	}
	if err != nil && !strings.Contains(err.Error(), "no-such-host") {
		t.Errorf("err = %v, want host name in message", err)
	}
}

func TestRenderer_FailsWithoutMemoryController(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	// Copy the fixture, but flip controllers to drop memory.
	yaml := `schema_version: "1.0.0"
fleet:
  hosts:
    - name: fsn-1
      role: single-box
daemons:
  schedd: {bind: unix:///run/faas/schedd.sock}
overlay:
  provider: tailscale
  cidr: 10.42.0.0/24
dns:
  apps_domain: apps.gregale.dev
  mode: cloudflare
postgresql:
  dsn: postgres://localhost/faas
  database: faas
  migration_max_slot: 1
  policy: on-boot
release:
  id: v1.0.0
  git_sha: 0123456789abcdef0123456789abcdef01234567
  architecture: x86_64
  firecracker_version: "1.10.0"
  firecracker_digest: ` + strings.Repeat("a", 64) + `
  kernel_digest: ` + strings.Repeat("b", 64) + `
  builder_base_digest: ` + strings.Repeat("c", 64) + `
  runtime_base_digest: ` + strings.Repeat("d", 64) + `
storage:
  fast_root: /srv/fc
  spool_root: /var/spool/faas
  log_root: /var/log/faas
  run_dir: /run/faas
cgroups:
  slice: faas-cp.slice
  controllers: "cpu,io,pids"
pki:
  root_dir: /etc/faas/tls
  ca_fingerprint: ` + strings.Repeat("e", 64) + `
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The validator already rejects a missing-memory-c manifest
	// (PR-0 carve-out). The renderer's invariant is that the
	// validator runs BEFORE any renderPKI/renderCgroup call.
	// Without the validator's check, the renderer would silently
	// emit a non-functional subtree_control. With the validator's
	// check, we surface the error at load-then-validate time.
	_, err := Render(RenderOptions{
		ManifestPath: path,
		DryRun:       true,
	})
	if err == nil {
		t.Errorf("missing-memory = nil err, want error")
	}
	if err != nil && !strings.Contains(err.Error(), "memory") {
		t.Errorf("err = %v, want 'memory' in message", err)
	}
}

func TestRenderer_ShortCircuitsOnIdempotentHash(t *testing.T) {
	// Run twice. First run writes; second run should report
	// unchanged + Action="unchanged" for every output.
	dir := t.TempDir()
	path := fixtureManifest(t, "fsn-1", "single-box")
	opts := RenderOptions{
		ManifestPath: path,
		ReleasesRoot: filepath.Join(dir, "releases"),
		EtcFaasDir:   filepath.Join(dir, "etc"),
		SystemdDir:   filepath.Join(dir, "systemd"),
		PKIRootDir:   filepath.Join(dir, "tls"),
		CgroupRoot:   filepath.Join(dir, "cgroup"),
	}

	// First run.
	r1, err := Render(opts)
	if err != nil {
		t.Fatalf("first Render: %v", err)
	}
	if r1.Skipped {
		t.Errorf("first run Skipped = true, want false")
	}

	// Second run: same opts, same on-disk content.
	r2, err := Render(opts)
	if err != nil {
		t.Fatalf("second Render: %v", err)
	}
	if !r2.Skipped {
		t.Errorf("second run Skipped = false, want true (idempotent)")
	}
	for _, o := range r2.Outputs {
		if o.Action != "unchanged" {
			t.Errorf("second run %s.Action = %q, want unchanged", o.Path, o.Action)
		}
	}
}

func TestRenderer_ResolvesControlPlaneDaemons(t *testing.T) {
	dir := t.TempDir()
	path := fixtureManifest(t, "fsn-1", "control-plane")
	opts := RenderOptions{
		ManifestPath: path,
		Host:         "fsn-1",
		EtcFaasDir:   filepath.Join(dir, "etc"),
		SystemdDir:   filepath.Join(dir, "systemd"),
		PKIRootDir:   filepath.Join(dir, "tls"),
		CgroupRoot:   filepath.Join(dir, "cgroup"),
		DryRun:       true,
	}
	report, err := Render(opts)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// Schema-drift guard: a control-plane box MUST NOT emit
	// vmmd.toml / faas-vmmd.service (those belong on a compute-
	// only box). Failure here is the canonical "role filter
	// regressed" signal.
	for _, mustNotContain := range []string{
		"vmmd.toml",
		"faas-vmmd.service",
		"builderd.toml",
		"imaged.toml",
		"gatewayd-internal.toml",
	} {
		for _, o := range report.Outputs {
			if strings.Contains(o.Path, mustNotContain) {
				t.Errorf("control-plane box must not emit %s, got %s", mustNotContain, o.Path)
			}
		}
	}
	// And it MUST emit the control-plane set.
	gotPaths := map[string]bool{}
	for _, o := range report.Outputs {
		gotPaths[filepath.Base(o.Path)] = true
	}
	for _, want := range []string{
		"apid.toml", "schedd.toml", "meterd.toml", "githubd.toml",
		"gatewayd-public.toml", "faas-cp.slice",
	} {
		if !gotPaths[want] {
			t.Errorf("control-plane box missing %s in output", want)
		}
	}
}

func TestRenderer_ResolvesComputeOnlyDaemons(t *testing.T) {
	dir := t.TempDir()
	path := fixtureManifest(t, "fsn-2", "compute-only")
	opts := RenderOptions{
		ManifestPath: path,
		Host:         "fsn-2",
		EtcFaasDir:   filepath.Join(dir, "etc"),
		SystemdDir:   filepath.Join(dir, "systemd"),
		PKIRootDir:   filepath.Join(dir, "tls"),
		CgroupRoot:   filepath.Join(dir, "cgroup"),
		DryRun:       true,
	}
	report, err := Render(opts)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// Schema-drift guard: a compute-only box MUST NOT emit
	// apid / schedd / meterd / githubd / gatewayd-public
	// TOMLs or service units.
	for _, mustNotContain := range []string{
		"apid.toml", "schedd.toml", "meterd.toml",
		"githubd.toml", "gatewayd-public.toml",
		"faas-apid.service", "faas-schedd.service",
	} {
		for _, o := range report.Outputs {
			if strings.Contains(o.Path, mustNotContain) {
				t.Errorf("compute-only box must not emit %s, got %s", mustNotContain, o.Path)
			}
		}
	}
	// And it MUST emit the compute-only set. (builderd is NOT
	// in the registry because vmmd spawns it per-build; the
	// renderer's role filter mirrors the registry.)
	gotPaths := map[string]bool{}
	for _, o := range report.Outputs {
		gotPaths[filepath.Base(o.Path)] = true
	}
	for _, want := range []string{
		"vmmd.toml", "imaged.toml", "gatewayd-internal.toml",
		"faas-cp.slice",
	} {
		if !gotPaths[want] {
			t.Errorf("compute-only box missing %s in output", want)
		}
	}
}

func TestRenderer_RequiresAbsolutePaths(t *testing.T) {
	path := fixtureManifest(t, "fsn-1", "single-box")
	_, err := Render(RenderOptions{
		ManifestPath: path,
		EtcFaasDir:   "relative/path",
	})
	if err == nil {
		t.Errorf("relative path = nil err, want error")
	}
	if err != nil && !strings.Contains(err.Error(), "absolute") {
		t.Errorf("err = %v, want 'absolute' in message", err)
	}
}

func TestRenderer_RequiresManifestPath(t *testing.T) {
	_, err := Render(RenderOptions{})
	if err == nil {
		t.Errorf("empty ManifestPath = nil err, want error")
	}
}

// TestRenderer_DaemonConfigFor round-trips the registry-name ↔
// manifest-name bridge. Every registry daemon must map to a
// manifest.DaemonConfig field.
func TestRenderer_DaemonConfigFor(t *testing.T) {
	m := &manifest.Manifest{
		Daemons: manifest.Daemons{
			Schedd:           &manifest.DaemonConfig{},
			Vmmd:             &manifest.DaemonConfig{},
			Apid:             &manifest.DaemonConfig{},
			Meterd:           &manifest.DaemonConfig{},
			Githubd:          &manifest.DaemonConfig{},
			GatewaydInternal: &manifest.DaemonConfig{},
			GatewaydPublic:   &manifest.DaemonConfig{},
			Imaged:           &manifest.DaemonConfig{},
			Builderd:         &manifest.DaemonConfig{},
		},
	}
	for _, name := range []string{
		"vmmd", "apid", "schedd", "meterd", "githubd",
		"gatewayd-internal", "gatewayd-public", "builderd", "imaged",
	} {
		dc := daemonConfigFor(m, name)
		if dc == nil {
			t.Errorf("daemonConfigFor(%q) = nil", name)
		}
	}
	if dc := daemonConfigFor(m, "no-such-daemon"); dc != nil {
		t.Errorf("daemonConfigFor(unsupported) = %v, want nil", dc)
	}
}

func TestFilterDaemons(t *testing.T) {
	got := filterDaemons([]string{"vmmd", "apid", "schedd", "meterd"}, "vmmd", "schedd")
	want := []string{"vmmd", "schedd"}
	if !equalStrings(got, want) {
		t.Errorf("filterDaemons = %v, want %v", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
