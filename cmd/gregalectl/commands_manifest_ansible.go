package main

import (
	"bytes"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/onebox-faas/faas/pkg/manifest"
)

// manifestAnsibleFile is one deterministic generated artifact. Keeping the
// generation pure makes the command safe to dry-run and gives the tests a
// direct contract without invoking Ansible or touching a live host.
type manifestAnsibleFile struct {
	Path string
	Body []byte
}

type manifestInternalHost struct {
	Address string
	Name    string
}

// cmdManifestAnsible materialises the Ansible inventory shape from the same
// manifest that drives the on-host renderer. The generated inventory is
// intentionally separate from deploy/ansible/inventory/ so a fleet can use
// a manifest-specific directory without editing committed IPs or host_vars.
// It emits only the production split-box groups; the retired one-box Ansible
// path is deliberately not representable here.
func cmdManifestAnsible(args []string) int {
	fs := flag.NewFlagSet("manifest ansible", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	manifestFile := fs.String("manifest-file", "", "path to the manifest YAML file (required)")
	outputDir := fs.String("output-dir", "", "generated Ansible root (required)")
	force := fs.Bool("force", false, "replace differing generated files")
	dryRun := fs.Bool("dry-run", false, "print planned files without writing")
	jsonOut := fs.Bool("json", false, "emit structured JSON to stdout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *manifestFile == "" || *outputDir == "" {
		fmt.Fprintln(os.Stderr, "gregalectl manifest ansible: --manifest-file and --output-dir are required")
		return 2
	}

	m, err := manifest.Load(*manifestFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gregalectl manifest ansible: load: %v\n", err)
		return 3
	}
	if errs := m.Validate(); errs != nil {
		fmt.Fprintf(os.Stderr, "gregalectl manifest ansible: invalid manifest: %s\n", errs)
		return 1
	}
	files, err := renderManifestAnsibleFiles(m, *outputDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gregalectl manifest ansible: %v\n", err)
		return 1
	}

	written := make([]string, 0, len(files))
	for _, file := range files {
		if *dryRun {
			written = append(written, file.Path)
			continue
		}
		if err := writeGeneratedAnsibleFile(file.Path, file.Body, *force); err != nil {
			fmt.Fprintf(os.Stderr, "gregalectl manifest ansible: %v\n", err)
			return 1
		}
		written = append(written, file.Path)
	}

	if *jsonOut || jsonOutput {
		jsonEmit(os.Stdout, struct {
			Manifest string   `json:"manifest"`
			Output   string   `json:"output_dir"`
			Files    []string `json:"files"`
			DryRun   bool     `json:"dry_run"`
		}{*manifestFile, *outputDir, written, *dryRun})
		return 0
	}
	for _, path := range written {
		_, _ = fmt.Fprintln(os.Stdout, path)
	}
	return 0
}

func renderManifestAnsibleFiles(m *manifest.Manifest, outputDir string) ([]manifestAnsibleFile, error) {
	if !filepath.IsAbs(outputDir) {
		return nil, fmt.Errorf("output-dir must be absolute to avoid writing inventory relative to an unexpected working directory")
	}
	if len(m.Fleet.Hosts) == 0 {
		return nil, fmt.Errorf("manifest declares no hosts")
	}
	for _, host := range m.Fleet.Hosts {
		if host.Role != roleControlPlane && host.Role != roleComputeOnly {
			return nil, fmt.Errorf("host %s has unsupported production role %q; use control-plane or compute-only", host.Name, host.Role)
		}
	}
	internalHosts, err := renderManifestInternalHosts(m)
	if err != nil {
		return nil, err
	}

	var controlPlane, computeOnly []string
	var hostVars []manifestAnsibleFile
	var postgresListenAddress string
	var postgresAllowedCIDRs []string
	for _, fleetHost := range m.Fleet.Hosts {
		if fleetHost.Role == roleControlPlane {
			address, _, parseErr := manifest.ParseHostPort(fleetHost.Address)
			if parseErr != nil {
				return nil, fmt.Errorf("host %s postgres address: %w", fleetHost.Name, parseErr)
			}
			if _, err := netip.ParseAddr(address); err == nil {
				postgresListenAddress = address
			}
		}
		if fleetHost.Role == roleComputeOnly {
			address, _, parseErr := manifest.ParseHostPort(fleetHost.Address)
			if parseErr != nil {
				return nil, fmt.Errorf("host %s postgres allow address: %w", fleetHost.Name, parseErr)
			}
			if _, err := netip.ParseAddr(address); err == nil {
				postgresAllowedCIDRs = append(postgresAllowedCIDRs, address+"/32")
			}
		}
	}
	for _, host := range m.Fleet.Hosts {
		targetURL := ""
		ansibleHost, _, parseErr := manifest.ParseHostPort(host.Address)
		if parseErr != nil {
			return nil, fmt.Errorf("host %s: %w", host.Name, parseErr)
		}
		if host.Role == "compute-only" {
			targetURL, parseErr = manifest.ServiceTCPURL(host.Role, host.Address)
			if parseErr != nil {
				return nil, fmt.Errorf("host %s target: %w", host.Name, parseErr)
			}
		}
		switch host.Role {
		case roleControlPlane:
			controlPlane = append(controlPlane, host.Name)
		case roleComputeOnly:
			computeOnly = append(computeOnly, host.Name)
		default:
			return nil, fmt.Errorf("host %s has unsupported production role %q; use control-plane or compute-only", host.Name, host.Role)
		}
		overlayCIDRs := ""
		if host.Role == roleComputeOnly {
			// The manifest's overlay CIDR is the fleet-wide network contract.
			// Only compute boxes route tenant traffic through that overlay;
			// the control plane keeps the canonical empty list.
			overlayCIDRs = m.Overlay.CIDR
		}
		body := renderManifestHostVars(host, ansibleHost, targetURL, internalHosts, overlayCIDRs, m.Overlay.Provider, postgresListenAddress, postgresAllowedCIDRs)
		hostVars = append(hostVars, manifestAnsibleFile{
			Path: filepath.Join(outputDir, "inventory", "host_vars", host.Name+".yml"),
			Body: []byte(body),
		})
	}

	var inventory bytes.Buffer
	inventory.WriteString("# Generated by gregalectl manifest ansible; do not hand-edit.\n")
	writeInventoryGroup(&inventory, "control_plane", controlPlane)
	writeInventoryGroup(&inventory, "compute_nodes", computeOnly)
	files := []manifestAnsibleFile{{
		Path: filepath.Join(outputDir, "inventory", "hosts.ini"),
		Body: inventory.Bytes(),
	}}
	files = append(files, hostVars...)
	return files, nil
}

func renderManifestInternalHosts(m *manifest.Manifest) ([]manifestInternalHost, error) {
	internalHosts := make([]manifestInternalHost, 0, len(m.Fleet.Hosts))
	for _, host := range m.Fleet.Hosts {
		serviceName, err := manifest.ServiceName(host.Role)
		if err != nil {
			return nil, fmt.Errorf("host %s: internal service identity: %w", host.Name, err)
		}
		address, _, err := manifest.ParseHostPort(host.Address)
		if err != nil {
			return nil, fmt.Errorf("host %s: internal service address: %w", host.Name, err)
		}
		// Literal overlay IPs need an explicit hosts entry because the
		// private PKI identity is not public Cloudflare DNS. A hostname
		// endpoint is left to the operator's private DNS contract.
		if _, err := netip.ParseAddr(address); err == nil {
			internalHosts = append(internalHosts, manifestInternalHost{
				Address: address,
				Name:    serviceName,
			})
		}
	}
	return internalHosts, nil
}

func writeInventoryGroup(out *bytes.Buffer, group string, hosts []string) {
	out.WriteString("[")
	out.WriteString(group)
	out.WriteString("]\n")
	for _, host := range hosts {
		out.WriteString(host)
		out.WriteByte('\n')
	}
	out.WriteByte('\n')
}

func renderManifestHostVars(host manifest.Host, ansibleHost, targetURL string, internalHosts []manifestInternalHost, overlayCIDRs, overlayProvider, postgresListenAddress string, postgresAllowedCIDRs []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Generated from the split-box manifest for %s; do not hand-edit.\n", host.Name)
	fmt.Fprintf(&b, "faas_box_role: %s\n", host.Role)
	fmt.Fprintf(&b, "faas_node_name: %s\n", host.Name)
	fmt.Fprintf(&b, "ansible_host: %q\n", ansibleHost)
	b.WriteString("ansible_python_interpreter: /usr/bin/python3\n")
	fmt.Fprintf(&b, "faas_overlay_provider: %q\n", overlayProvider)
	switch overlayProvider {
	case "tailscale":
		b.WriteString("faas_overlay_iface: tailscale0\n")
	case "wireguard":
		b.WriteString("faas_overlay_iface: wg0\n")
	case "static":
		b.WriteString("faas_overlay_iface: \"{{ ansible_default_ipv4.interface | default('eth0') }}\"\n")
	}
	if host.Role == roleComputeOnly && overlayCIDRs != "" {
		fmt.Fprintf(&b, "overlay_cidrs: [%q]\n", overlayCIDRs)
	} else {
		b.WriteString("overlay_cidrs: []\n")
	}
	if len(internalHosts) > 0 {
		b.WriteString("faas_internal_hosts:\n")
		for _, internalHost := range internalHosts {
			fmt.Fprintf(&b, "  - address: %q\n    names: [%q]\n", internalHost.Address, internalHost.Name)
		}
	}
	if host.Role == roleComputeOnly {
		b.WriteString("faas_vmmd_listen_addr: \"tcp://0.0.0.0:50051\"\n")
		fmt.Fprintf(&b, "faas_vmmd_target_url: %q\n", targetURL)
	}
	if host.Role == roleControlPlane && postgresListenAddress != "" {
		fmt.Fprintf(&b, "faas_postgres_listen_addresses: %q\n", postgresListenAddress)
		if len(postgresAllowedCIDRs) == 0 {
			b.WriteString("faas_postgres_allowed_cidrs: []\n")
		} else {
			fmt.Fprintf(&b, "faas_postgres_allowed_cidrs: [%s]\n", quotedYAMLList(postgresAllowedCIDRs))
		}
	}
	return b.String()
}

func quotedYAMLList(values []string) string {
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = fmt.Sprintf("%q", value)
	}
	return strings.Join(quoted, ", ")
}

func writeGeneratedAnsibleFile(path string, body []byte, force bool) error {
	if existing, err := os.ReadFile(path); err == nil {
		if bytes.Equal(existing, body) {
			return nil
		}
		if !force {
			return fmt.Errorf("refusing to overwrite differing generated file %s; use --force", path)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	return os.WriteFile(path, body, 0o644)
}
