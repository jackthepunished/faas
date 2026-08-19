package renderer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/onebox-faas/faas/pkg/daemonunitspec"
	"github.com/onebox-faas/faas/pkg/manifest"
)

// RenderOptions is the operator-supplied input set for a single
// renderer run. Defaults are applied on the Resolve step that
// precedes Render; the public Render() entry point accepts the
// fields with defaults already populated so caller-side helpers
// (the CLI leaf, the e2e test) can populate explicitly.
type RenderOptions struct {
	// ManifestPath is the path to the cluster manifest YAML.
	ManifestPath string

	// Host is the manifest host to render for. If empty, the
	// first host in the manifest is used (single-box fallback).
	Host string

	// ReleasesRoot is the directory holding the per-release
	// binaries. Default: /opt/faas/releases.
	ReleasesRoot string

	// EtcFaasDir is the directory for the per-daemon TOML files.
	// Default: /etc/faas.
	EtcFaasDir string

	// SystemdDir is the directory for the systemd units and the
	// faas-cp.slice slice unit. Default: /etc/systemd/system.
	SystemdDir string

	// PKIRootDir is the directory for the per-host PKI leaves.
	// Default: /etc/faas/tls.
	PKIRootDir string

	// CgroupRoot is the cgroup v2 mount root. Default: /sys/fs/cgroup.
	CgroupRoot string

	// HostSANFile is an optional sidecar JSON file with per-host
	// SANs. Used by the vmmd [compute_node] SAN list. Empty
	// means no extra SANs (single-box default).
	HostSANFile string

	// DryRun short-circuits all filesystem writes. The renderer
	// still validates the manifest + runs the TOML placement check
	// + computes the sha256s, but every publish is replaced with
	// an OutputReport.Action = "skipped" with the computed digest.
	DryRun bool
}

// RenderReport is the JSON wire shape for `gregale manifest render
// --json`. Mirrors the manifest validate report's shape so a single
// jq pattern handles both. The Skipped bool flips true on a
// second-run idempotent short-circuit where every output's on-disk
// sha256 matches the rendered sha256.
//
// Role is the manifest host's role (single-box / control-plane /
// compute-only) — PR-2 stores it on compute_nodes.role after the
// file-write phase. Empty Role means the driver doesn't know
// (single-box dev where the manifest host name is empty).
type RenderReport struct {
	Host         string         `json:"host"`
	Role         string         `json:"role,omitempty"`
	ManifestHash string         `json:"manifest_hash"`
	Outputs      []OutputReport `json:"outputs"`
	Skipped      bool           `json:"skipped"`
	Audit        []string       `json:"audit,omitempty"`
}

// OutputReport is the per-file stamp emitted in RenderReport.Outputs.
// The Action field pins the operator-visible "wrote" / "skipped" /
// "unchanged" attribution so the CI gate can distinguish a fresh
// publish from a no-op idempotent short-circuit.
type OutputReport struct {
	Path   string `json:"path"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
	Action string `json:"action"` // "wrote" | "skipped" | "unchanged"
}

// resolveDefaults populates the unset fields with their canonical
// production paths. The renderer is operator-only; missing
// ReleasesRoot on a clean dev box means the operator forgot to
// override — surface the error rather than silently defaulting.
func (o RenderOptions) resolveDefaults() (RenderOptions, error) {
	if o.ManifestPath == "" {
		return o, fmt.Errorf("renderer: ManifestPath is required")
	}
	if o.ReleasesRoot == "" {
		o.ReleasesRoot = "/opt/faas/releases"
	}
	if o.EtcFaasDir == "" {
		o.EtcFaasDir = "/etc/faas"
	}
	if o.SystemdDir == "" {
		o.SystemdDir = "/etc/systemd/system"
	}
	if o.PKIRootDir == "" {
		o.PKIRootDir = "/etc/faas/tls"
	}
	if o.CgroupRoot == "" {
		o.CgroupRoot = "/sys/fs/cgroup"
	}
	if !filepath.IsAbs(o.ReleasesRoot) || !filepath.IsAbs(o.EtcFaasDir) ||
		!filepath.IsAbs(o.SystemdDir) || !filepath.IsAbs(o.PKIRootDir) ||
		!filepath.IsAbs(o.CgroupRoot) {
		return o, fmt.Errorf("renderer: ReleasesRoot / EtcFaasDir / SystemdDir / PKIRootDir / CgroupRoot must all be absolute paths")
	}
	return o, nil
}

// RenderEntry is the public entry point. The function signature
// accepts the resolved options so caller-side helpers (CLI leaf,
// e2e test) can apply their own defaults before invoking.
func RenderEntry(opts RenderOptions) (RenderReport, error) {
	resolved, err := opts.resolveDefaults()
	if err != nil {
		return RenderReport{}, err
	}
	return render(resolved)
}

// Render is the package alias for RenderEntry. The redundancy
// matches the rest of the codebase (e.g. pkg/releaseinstall.Build
// vs the package-private build).
func Render(opts RenderOptions) (RenderReport, error) {
	return RenderEntry(opts)
}

// render is the internal implementation. It loads the manifest,
// validates it, resolves the host, then walks the five outputs:
//
//  1. /etc/faas/<daemon>.toml              (per daemon on host)
//  2. /etc/systemd/system/faas-<daemon>.service   (per daemon)
//  3. /etc/systemd/system/faas-cp.slice     (once per host)
//  4. /etc/faas/tls/<role>/<file>.{crt,key} (per role in RolesForBox)
//  5. /sys/fs/cgroup/<slice>/subtree_control (once per host)
//
// Phase 1: load + validate. NO filesystem writes if the manifest
// is invalid. Phase 2: compute all outputs in memory and verify
// each daemon's TOML placement is valid. Phase 3: publish atomically.
// If a publish fails mid-flight, the error is returned and the
// caller sees a partial RenderReport (so the operator can see which
// files landed and which didn't).
func render(opts RenderOptions) (RenderReport, error) {
	// Phase 1: load + validate.
	m, err := manifest.Load(opts.ManifestPath)
	if err != nil {
		return RenderReport{}, fmt.Errorf("renderer: load %s: %w", opts.ManifestPath, err)
	}
	if errs := m.Validate(); errs != nil {
		return RenderReport{}, fmt.Errorf("renderer: invalid manifest: %s", errs.Error())
	}
	manifestHash, err := hashManifestFile(opts.ManifestPath)
	if err != nil {
		return RenderReport{}, err
	}

	// Phase 2: resolve host.
	host, err := resolveHost(m, opts.Host)
	if err != nil {
		return RenderReport{}, err
	}

	// Per-host SANs from the sidecar JSON. Empty list is fine
	// (single-box / dev installs).
	hostSANs, err := loadHostSANs(opts.HostSANFile)
	if err != nil {
		return RenderReport{}, err
	}

	// Resolve the per-host daemon set. The registry has 8 daemons;
	// the host's role filters them out (we never short-circuit by
	// Critical — best-effort daemons still ship on the host they
	// run on).
	//
	// Note: builderd is NOT in the registry because it is not a
	// long-running systemd service — vmmd spawns it per-build inside
	// an ephemeral builder microVM (ADR-003). The registry shape
	// (8 daemons) is correct; the renderer's role filter must mirror
	// it (no "builderd" entry — adding one would be a no-op against
	// ActivationOrder() and a footgun for future readers).
	daemons := daemonunitspec.ActivationOrder()
	switch host.Role {
	case "compute-only":
		// Compute-only boxes run vmmd, imaged, and gatewayd-internal.
		// They do NOT run apid, schedd, meterd, githubd, or
		// gatewayd-public. (builderd is per-build via vmmd; not a
		// systemd unit — see comment above.)
		daemons = filterDaemons(daemons, "vmmd", "imaged", "gatewayd-internal")
	case "control-plane":
		// Control-plane boxes run apid, schedd, meterd, githubd,
		// gatewayd-public. They do NOT run vmmd, imaged, or
		// gatewayd-internal.
		daemons = filterDaemons(daemons, "apid", "schedd", "meterd", "githubd", "gatewayd-public")
	}
	// single-box (and ""): all 8 (Registry already includes imaged).

	// Compute every output in memory first. Phase 3 publishes them
	// atomically. The two-phase approach makes the renderer
	// fail-fast: any output that fails to validate (e.g. a
	// tombstone hit) aborts the publish phase with no partial
	// state.
	type computedOutput struct {
		path string
		body []byte
		mode os.FileMode
	}
	var outputs []computedOutput
	report := RenderReport{Host: host.Name, Role: host.Role, ManifestHash: manifestHash}

	// 1. /etc/faas/<daemon>.toml  +  2. /etc/systemd/system/faas-<daemon>.service
	for _, d := range daemons {
		dc := daemonConfigFor(m, d)
		if dc == nil {
			// Manifest omits this daemon's block. The renderer
			// tolerates a missing block (a single-box host with
			// only schedd declared would still emit systemd
			// units for the omitted daemons). Surface this in
			// Audit rather than aborting.
			report.Audit = append(report.Audit, fmt.Sprintf("manifest.daemons.%s omitted; emitting systemd unit without TOML config", d))
			dc = &manifest.DaemonConfig{}
		}
		// The renderer names daemons with dashes (gatewayd-internal,
		// gatewayd-public) for the registry + on-disk shape; the
		// manifest's HostKeys uses underscores. The translation
		// is the bridge the renderer owns. renderTOML accepts
		// either form via registryToHostKey.
		tomlBody, _, err := renderTOML(tomlRenderCtx{
			Daemon:      d,
			DC:          dc,
			AppsDomain:  m.DNS.AppsDomain,
			HostSANs:    hostSANs,
			HostName:    host.Name,
			HostAddress: host.Address,
		})
		if err != nil {
			return report, err
		}
		tomlPath := filepath.Join(opts.EtcFaasDir, d+".toml")
		outputs = append(outputs, computedOutput{path: tomlPath, body: tomlBody, mode: 0o644})

		// PR-2 (issue #911 / ADR-110): thread host.Name so the
		// rendered unit carries Environment=FAAS_NODE_NAME=<host>.
		// Empty host.Name (single-box dev) emits no Environment= line
		// — the daemon's TOML default applies and the unit boots
		// without a multi-host identity anchor.
		systemdBody, err := renderSystemd(d, host.Name)
		if err != nil {
			return report, err
		}
		systemdPath := filepath.Join(opts.SystemdDir, "faas-"+d+".service")
		outputs = append(outputs, computedOutput{path: systemdPath, body: systemdBody, mode: 0o644})
	}

	// 3. /etc/systemd/system/faas-cp.slice
	sliceBody := renderSliceUnit()
	slicePath := filepath.Join(opts.SystemdDir, "faas-cp.slice")
	outputs = append(outputs, computedOutput{path: slicePath, body: sliceBody, mode: 0o644})

	// 4. /etc/faas/tls/<role>/<file>.{crt,key}
	// renderPKI ensures the leaves exist on disk. The renderer
	// returns one PKIOutput per leaf (Issued=true on first run,
	// Issued=false on the idempotent second run). Every leaf
	// gets an OutputReport entry regardless — the doctor's
	// PKI-health signal depends on every leaf being visible.
	// The PKI OutputReports are emitted AFTER the publish loop
	// (below) so anyWritten is computed first; we just stash the
	// PKI outputs here.
	var pkiOutputs []OutputReport
	if !opts.DryRun {
		leafOutputs, err := renderPKI(opts.PKIRootDir, host.Role)
		if err != nil {
			return report, err
		}
		for _, lo := range leafOutputs {
			action := "unchanged"
			if lo.Issued {
				action = "wrote"
			}
			pkiOutputs = append(pkiOutputs, OutputReport{
				Path:   lo.Path,
				Action: action,
			})
		}
	}

	// 5. /sys/fs/cgroup/<slice>/subtree_control
	cgroupBody, cgroupErr := renderCgroupBody(m.Cgroups.Slice, m.Cgroups.Controllers)
	if cgroupErr != nil {
		return report, cgroupErr
	}
	cgroupPath := filepath.Join(opts.CgroupRoot, m.Cgroups.Slice, "subtree_control")
	if !opts.DryRun {
		// Ensure the slice directory exists so publishAtomic's
		// tmp + rename can target it. MkdirAll is a no-op on a
		// fresh slice after the first run.
		if err := os.MkdirAll(filepath.Join(opts.CgroupRoot, m.Cgroups.Slice), 0o755); err != nil {
			return report, fmt.Errorf("renderer: cgroup: mkdir %s: %w", filepath.Join(opts.CgroupRoot, m.Cgroups.Slice), err)
		}
	}
	outputs = append(outputs, computedOutput{path: cgroupPath, body: cgroupBody, mode: 0o644})

	// Phase 3: publish each computed output. The idempotent
	// short-circuit uses publishAtomic's digest comparison.
	report.Skipped = true
	anyWritten := false
	for _, co := range outputs {
		if opts.DryRun {
			report.Outputs = append(report.Outputs, OutputReport{
				Path:   co.path,
				Bytes:  len(co.body),
				SHA256: sha256Hex(co.body),
				Action: "skipped",
			})
			continue
		}
		digest, changed, err := publishAtomic(co.path, co.body, co.mode)
		if err != nil {
			return report, err
		}
		action := "wrote"
		if !changed {
			action = "unchanged"
		} else {
			anyWritten = true
		}
		report.Outputs = append(report.Outputs, OutputReport{
			Path:   co.path,
			Bytes:  len(co.body),
			SHA256: digest,
			Action: action,
		})
	}
	// Append PKI outputs after the publish loop so every leaf is
	// visible in the report on every run (idempotent or not). The
	// doctor's PKI-health signal depends on this visibility.
	// PKI leaves also count toward anyWritten — re-issuing a leaf
	// IS a write for Skipped semantics.
	for _, pkiOut := range pkiOutputs {
		report.Outputs = append(report.Outputs, pkiOut)
		if pkiOut.Action == "wrote" {
			anyWritten = true
		}
	}
	// Skipped is true only if no writes happened (idempotent).
	// Any actual write flips it to false.
	if anyWritten {
		report.Skipped = false
	}

	// Install /opt/faas/current symlink. The current symlink is
	// sibling-of-releases, not inside; pkg/releaseinstall.AtomicFlip
	// is the same pattern, but the renderer does not import that
	// package. The release-id segment is the manifest's
	// release.id (e.g. "v1.4.0-12-gabc1234") — the renderer
	// doesn't synthesise a git_sha from the manifest.
	if !opts.DryRun {
		target := filepath.Join(opts.ReleasesRoot, m.Release.ID)
		currentPath := filepath.Join(filepath.Dir(opts.ReleasesRoot), "current")
		if err := installCurrentSymlink(currentPath, target); err != nil {
			return report, err
		}
	}

	// Stable output order for the JSON report.
	sort.SliceStable(report.Outputs, func(i, j int) bool {
		return report.Outputs[i].Path < report.Outputs[j].Path
	})
	return report, nil
}

// resolveHost picks the host to render for. Single-box manifests
// have exactly one host, so the default is the first entry. Multi-
// box manifests require the operator to name the host explicitly;
// the renderer surfaces a missing-host error in that case.
func resolveHost(m *manifest.Manifest, name string) (manifest.Host, error) {
	if len(m.Fleet.Hosts) == 0 {
		return manifest.Host{}, fmt.Errorf("renderer: manifest declares zero hosts")
	}
	if name == "" {
		return m.Fleet.Hosts[0], nil
	}
	for _, h := range m.Fleet.Hosts {
		if h.Name == name {
			return h, nil
		}
	}
	return manifest.Host{}, fmt.Errorf("renderer: host %q not found in manifest (declared: %s)", name, hostNames(m))
}

func hostNames(m *manifest.Manifest) string {
	out := make([]string, len(m.Fleet.Hosts))
	for i, h := range m.Fleet.Hosts {
		out[i] = h.Name
	}
	return strings.Join(out, ", ")
}

// daemonConfigFor returns the manifest's DaemonConfig for the given
// daemon name. The manifest's Daemons struct uses underscored names
// for gatewayd_internal and gatewayd_public; the registry uses
// dashed names. The lookup bridges the two.
func daemonConfigFor(m *manifest.Manifest, registryName string) *manifest.DaemonConfig {
	switch registryName {
	case "vmmd":
		return m.Daemons.Vmmd
	case "apid":
		return m.Daemons.Apid
	case "schedd":
		return m.Daemons.Schedd
	case "meterd":
		return m.Daemons.Meterd
	case "githubd":
		return m.Daemons.Githubd
	case "gatewayd-internal":
		return m.Daemons.GatewaydInternal
	case "gatewayd-public":
		return m.Daemons.GatewaydPublic
	case "builderd":
		return m.Daemons.Builderd
	case "imaged":
		return m.Daemons.Imaged
	}
	return nil
}

// registryToHostKey translates the registry's dashed daemon name
// (gatewayd-internal, gatewayd-public) to the manifest.HostKeys
// catalog's underscored form (gatewayd_internal, gatewayd_public).
// Other daemons are unchanged across the two namespaces.
func registryToHostKey(registryName string) string {
	switch registryName {
	case "gatewayd-internal":
		return "gatewayd_internal"
	case "gatewayd-public":
		return "gatewayd_public"
	}
	return registryName
}

// filterDaemons returns the subset of names that match any of the
// keep values. Used to project the registry's 8 daemons onto the
// per-host role filter.
func filterDaemons(names []string, keep ...string) []string {
	keepSet := make(map[string]bool, len(keep))
	for _, k := range keep {
		keepSet[k] = true
	}
	out := make([]string, 0, len(keep))
	for _, n := range names {
		if keepSet[n] {
			out = append(out, n)
		}
	}
	return out
}

// loadHostSANs reads the optional sidecar JSON file at path. The
// file shape is a single JSON array of strings. Returns ([], nil)
// for empty path.
func loadHostSANs(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("renderer: read host SAN file %s: %w", path, err)
	}
	var out []string
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("renderer: parse host SAN file %s: %w", path, err)
	}
	return out, nil
}
