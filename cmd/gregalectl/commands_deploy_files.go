// commands_deploy_files.go — host_vars + hosts.ini writers for
// `gregalectl deploy add-node` (PR-B to PR #935, multi-host scale-out
// gap #2).
//
// The Go-side template literals are the canonical source of
// truth for the YAML/INI shape — the existing host_vars/faas-fsn-{1,2}.yml
// files drift-tested against them via assertHostVarsShape (the
// test in commands_deploy_test.go walks both files and asserts
// the parsed output matches what the renderer would produce for
// the equivalent inputs).

package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// renderHostVarsYAML returns the canonical host_vars/<fqdn>.yml body
// for a given role. The shape mirrors the existing
// faas-fsn-{1,2}.yml files (issue #911 / ADR-110 deploy side); the
// renderer is the single source of truth so a future per-box
// field change lands in one place.
//
// The control-plane subset drops masquerade_cidr + masquerade_cidr_v6
// (no overlay traffic on the CP box — per host_vars/faas-fsn-1.yml:24-37).
// The compute-only subset carries them per host_vars/faas-fsn-2.yml:25-39.
func renderHostVarsYAML(fqdn, role, ansibleHost, publicIface, masqCIDR, masqCIDRv6, overlayCIDRs string) string {
	roleComment := "control-plane box"
	roleMarker := "control-plane"
	hosts := []string{"postgres", "scheduler", "metering", "gateway-public", "githubd"}
	if role == "compute-only" {
		roleComment = "compute-only box"
		roleMarker = "compute-only"
		hosts = []string{"vmmd", "gatewayd-internal", "builderd", "imaged"}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Gate-B (issue #297 / ADR-025 §Tier 2) %s.\n", roleComment)
	fmt.Fprintf(&b, "# %s runs %s.\n", fqdn, strings.Join(hosts, ", "))
	fmt.Fprintf(&b, "# Postgres + the Tier 1 PKI CA live on the control-plane box; this box dials\n")
	fmt.Fprintf(&b, "# schedd over the overlay network (Tail/wg) with the per-node client cert\n")
	fmt.Fprintf(&b, "# the role-appropriate `gregalectl pki init --box-role=%s` lays out under\n", roleMarker)
	fmt.Fprintf(&b, "# /etc/faas/tls/<role>/schedd-client.{crt,key}.\n")
	fmt.Fprintf(&b, "#\n")
	fmt.Fprintf(&b, "# The role gate (pkg/role, PR-1) refuses to start the wrong-role daemons;\n")
	fmt.Fprintf(&b, "# the var below is the deploy-time source of truth — every daemon's systemd\n")
	fmt.Fprintf(&b, "# unit reads it via FAAS_<DAEMON>_ROLE.\n")
	fmt.Fprintf(&b, "\n")
	fmt.Fprintf(&b, "faas_box_role: %s\n", roleMarker)
	fmt.Fprintf(&b, "faas_node_name: %s\n", fqdn)
	fmt.Fprintf(&b, "\n")
	fmt.Fprintf(&b, "# Ansible connection vars. `ansible_host` is the cross-box addressing\n")
	fmt.Fprintf(&b, "# target (Layer-3 mesh endpoint: Tailscale/Wireguard IP, internal LAN,\n")
	fmt.Fprintf(&b, "# or routable FQDN — operator-supplied per-fleet). The\n")
	fmt.Fprintf(&b, "# `ansible_python_interpreter` pin matches the Ubuntu 24.04 system\n")
	fmt.Fprintf(&b, "# python3.12 so we never depend on a venv.\n")
	fmt.Fprintf(&b, "ansible_host: %s\n", ansibleHost)
	fmt.Fprintf(&b, "ansible_python_interpreter: /usr/bin/python3\n")
	if role == "compute-only" {
		fmt.Fprintf(&b, "\n")
		fmt.Fprintf(&b, "# nftables substitution: the public-iface is the outbound interface the\n")
		fmt.Fprintf(&b, "# postrouting chain MASQUERADES tenant overlay traffic on. Operators set\n")
		fmt.Fprintf(&b, "# this to the box's WAN-facing NIC (e.g. ens5 on Hetzner, eth0 on bare metal).\n")
		fmt.Fprintf(&b, "public_iface: %s\n", publicIface)
		fmt.Fprintf(&b, "masquerade_cidr: %s\n", masqCIDR)
		if masqCIDRv6 != "" {
			fmt.Fprintf(&b, "masquerade_cidr_v6: %s\n", masqCIDRv6)
		} else {
			fmt.Fprintf(&b, "masquerade_cidr_v6: ''\n")
		}
		fmt.Fprintf(&b, "\n")
		fmt.Fprintf(&b, "# Multi-host mesh (ADR-092). The overlay CIDRs list is the per-box\n")
		fmt.Fprintf(&b, "# slice of the /16 the nftables_postrouting/forward chains MASQUERADE.\n")
		fmt.Fprintf(&b, "# On a single-box dev layout this is empty (renders the canonical\n")
		fmt.Fprintf(&b, "# default-local ruleset byte-identical to the pre-Mega-PR-C output).\n")
		if overlayCIDRs == "" {
			fmt.Fprintf(&b, "overlay_cidrs: []\n")
		} else {
			fmt.Fprintf(&b, "overlay_cidrs: [%s]\n", overlayCIDRs)
		}
	} else {
		fmt.Fprintf(&b, "\n")
		fmt.Fprintf(&b, "# Multi-host mesh (ADR-092): the control-plane box does NOT route\n")
		fmt.Fprintf(&b, "# overlay traffic — it sets up Postgres + the CP-only daemons. The\n")
		fmt.Fprintf(&b, "# N+ overlay mesh runs between the compute_only boxes; nftables\n")
		fmt.Fprintf(&b, "# therefore sees zero overlay entries on this box and the template\n")
		fmt.Fprintf(&b, "# renders the default ruleset (no per-overlay accept, no per-overlay\n")
		fmt.Fprintf(&b, "# MASQUERADE). Empty list is the canonical \"no overlay\" sentinel —\n")
		fmt.Fprintf(&b, "# not \"0.0.0.0/0\".\n")
		fmt.Fprintf(&b, "overlay_cidrs: []\n")
	}
	return b.String()
}

// updateHostsINIAddNode is the idempotent hosts.ini writer. Adds
// <fqdn> to the appropriate group ([control_plane] or
// [compute_nodes]) if not already present. Leaves the rest of the
// file untouched. Returns the new file body.
//
// Why idempotent: re-running `deploy add-node <fqdn>` with the
// same fqdn must be a no-op (the dedup check + grep before insert).
// Otherwise the file accumulates duplicate lines every retry.
func updateHostsINIAddNode(path, fqdn, role string) ([]byte, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read hosts.ini: %w", err)
	}
	group := ansibleGroup(role)
	if stringContainsFQDN(body, fqdn) {
		// Already present — no-op, return the file unchanged.
		return body, nil
	}
	// Find the group header line and insert <fqdn> as the next
	// non-empty / non-comment line under it. The structure is:
	//   [group]
	//   <fqdn>
	//   <other-fqdn> ...
	//   (blank line)
	//   [next-group]
	groupHeader := "[" + group + "]"
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	var out strings.Builder
	inserted := false
	groupSeen := false
	for scanner.Scan() {
		line := scanner.Text()
		out.WriteString(line)
		out.WriteByte('\n')
		if !inserted && groupSeen && line != "" && !strings.HasPrefix(line, "#") && line != groupHeader {
			// We're inside the group section, on a data line.
			// Insert <fqdn> right after this one (preserves
			// existing ordering — new hosts append at the end).
			out.WriteString(fqdn)
			out.WriteByte('\n')
			inserted = true
		}
		if strings.TrimSpace(line) == groupHeader {
			groupSeen = true
			// Handle the degenerate case: an empty group section
			// (only the header followed by a blank line or EOF).
			// Lookahead: if the next non-blank line is the next
			// group header, insert <fqdn> directly under the
			// group's header.
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan hosts.ini: %w", err)
	}
	if !inserted {
		// Group section was empty (no entries) — append under the
		// header line. If the group section didn't exist at all,
		// this is a misconfigured hosts.ini and we surface that.
		if !groupSeen {
			return nil, fmt.Errorf("hosts.ini missing [%s] group header", group)
		}
		// Re-scan and insert directly under the header line.
		scanner = bufio.NewScanner(strings.NewReader(string(body)))
		out.Reset()
		groupSeen = false
		for scanner.Scan() {
			line := scanner.Text()
			if !inserted && groupSeen && strings.TrimSpace(line) == groupHeader {
				out.WriteString(line)
				out.WriteByte('\n')
				out.WriteString(fqdn)
				out.WriteByte('\n')
				inserted = true
				continue
			}
			out.WriteString(line)
			out.WriteByte('\n')
			if strings.TrimSpace(line) == groupHeader {
				groupSeen = true
			}
		}
	}
	return []byte(out.String()), nil
}

// stringContainsFQDN returns true if `fqdn` appears as a
// whitespace-delimited token anywhere in `body`. Avoids matching
// `fsn-1` inside `fsn-10`.
func stringContainsFQDN(body []byte, fqdn string) bool {
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		for _, field := range strings.Fields(line) {
			if field == fqdn {
				return true
			}
		}
	}
	return false
}
