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

	"github.com/onebox-faas/faas/pkg/roleTemplating"
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
//
// All operator-supplied string values are YAML-quoted via yamlQuote
// so a value containing `:`, `#`, `"`, or starting with `*&!|>'%@`
// can't break YAML parsing or smuggle anchors. The input fields come
// from CLI flags (--ansible-host, --public-iface, --masquerade-cidr…);
// an operator pasting a value with a typo (e.g. `10.42.0.3:50051` for
// `--ansible-host`) would otherwise produce `ansible_host: 10.42.0.3:50051`
// which parses as the key `ansible_host` with value `50051` and the rest
// as a string-typed fragment — ansible then fails to load the file.
func renderHostVarsYAML(fqdn, role, ansibleHost, publicIface, masqCIDR, masqCIDRv6, overlayCIDRs string) string {
	return renderHostVarsYAMLWithTargetURL(fqdn, role, ansibleHost, publicIface, masqCIDR, masqCIDRv6, overlayCIDRs, "")
}

// renderHostVarsYAMLWithTargetURL is the deployment coordinator's variant.
// A compute-only node's vmmd routing values are rendered into host_vars so
// Ansible can install the systemd drop-in that drives self-registration.
func renderHostVarsYAMLWithTargetURL(fqdn, role, ansibleHost, publicIface, masqCIDR, masqCIDRv6, overlayCIDRs, targetURL string) string {
	roleComment := roleControlPlane + " box"
	roleMarker := roleControlPlane
	canonicalNodeName := canonicalComputeNodeName(fqdn, roleTemplating.Role(role))
	hosts := []string{"postgres", "scheduler", "metering", "gateway-public", "githubd"}
	if role == roleComputeOnly {
		roleComment = roleComputeOnly + " box"
		roleMarker = roleComputeOnly
		hosts = []string{"vmmd", "gatewayd-internal", "builderd", "imaged"}
	}

	// Anchor `fqdn` and `roleMarker` are validated upstream by
	// validComputeNodeName / the role enum and never contain
	// shell-meaningful chars — left unquoted for human diff.
	// Everything else is operator-supplied and quoted.
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
	fmt.Fprintf(&b, "faas_node_name: %s\n", canonicalNodeName)
	fmt.Fprintf(&b, "\n")
	fmt.Fprintf(&b, "# Ansible connection vars. `ansible_host` is the cross-box addressing\n")
	fmt.Fprintf(&b, "# target (Layer-3 mesh endpoint: Tailscale/Wireguard IP, internal LAN,\n")
	fmt.Fprintf(&b, "# or routable FQDN — operator-supplied per-fleet). The\n")
	fmt.Fprintf(&b, "# `ansible_python_interpreter` pin matches the Ubuntu 24.04 system\n")
	fmt.Fprintf(&b, "# python3.12 so we never depend on a venv.\n")
	fmt.Fprintf(&b, "ansible_host: %s\n", yamlQuote(ansibleHost))
	fmt.Fprintf(&b, "ansible_python_interpreter: /usr/bin/python3\n")
	if role == roleComputeOnly {
		fmt.Fprintf(&b, "\n")
		fmt.Fprintf(&b, "# nftables substitution: the public-iface is the outbound interface the\n")
		fmt.Fprintf(&b, "# postrouting chain MASQUERADES tenant overlay traffic on. Operators set\n")
		fmt.Fprintf(&b, "# this to the box's WAN-facing NIC (e.g. ens5 on Hetzner, eth0 on bare metal).\n")
		fmt.Fprintf(&b, "public_iface: %s\n", yamlQuote(publicIface))
		fmt.Fprintf(&b, "masquerade_cidr: %s\n", yamlQuote(masqCIDR))
		if targetURL != "" {
			fmt.Fprintf(&b, "\n")
			fmt.Fprintf(&b, "# Multi-box vmmd routing. listen_addr is the bind target;\n")
			fmt.Fprintf(&b, "# target_url is the routable dial target written to compute_nodes.\n")
			fmt.Fprintf(&b, "faas_vmmd_listen_addr: %s\n", yamlQuote("tcp://0.0.0.0:50051"))
			fmt.Fprintf(&b, "faas_vmmd_target_url: %s\n", yamlQuote(targetURL))
		}
		if masqCIDRv6 != "" {
			fmt.Fprintf(&b, "masquerade_cidr_v6: %s\n", yamlQuote(masqCIDRv6))
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
			fmt.Fprintf(&b, "overlay_cidrs: %s\n", yamlQuoteList(overlayCIDRs))
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

// yamlQuote returns a YAML double-quoted scalar for v. Always
// quoted (even for already-safe values) so the diff is byte-stable
// across reformattings. Backslashes and embedded double-quotes are
// escaped per YAML 1.2 §5.2.
func yamlQuote(v string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range v {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// yamlQuoteList wraps a comma-separated list into a flow-style YAML
// sequence with each entry double-quoted. Empty input returns "[]".
func yamlQuoteList(csv string) string {
	if strings.TrimSpace(csv) == "" {
		return "[]"
	}
	var b strings.Builder
	b.WriteByte('[')
	first := true
	for _, f := range strings.Split(csv, ",") {
		if !first {
			b.WriteString(", ")
		}
		first = false
		b.WriteString(yamlQuote(strings.TrimSpace(f)))
	}
	b.WriteByte(']')
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
		trimmed := strings.TrimSpace(line)
		// Reset groupSeen on every group header — keeps the
		// "append under the right group" logic honest when an
		// earlier group was non-empty (the false-positive path
		// otherwise finds the new fqdn in the wrong group).
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			groupSeen = trimmed == groupHeader
		}
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
		// `groupSeen` is set the moment we hit the target group
		// header so the next iteration (which is the line UNDER
		// the header) can decide whether to insert here or fall
		// through to the data-line append path.
		groupSeen := false
		inserted = false
		for scanner.Scan() {
			line := scanner.Text()
			trimmed := strings.TrimSpace(line)
			if !inserted && groupSeen && trimmed == "" {
				// We're on the first blank line under the target
				// group header. Insert <fqdn> ahead of it: the
				// preserved ordering is [header]\n<fqdn>\n(blank).
				out.WriteString(fqdn)
				out.WriteByte('\n')
				inserted = true
			}
			out.WriteString(line)
			out.WriteByte('\n')
			if trimmed == groupHeader {
				groupSeen = true
			}
		}
		if !inserted {
			// Group was the last section in the file (no blank
			// line under it before EOF). Append at the end.
			out.WriteString(fqdn)
			out.WriteByte('\n')
		}
		// `inserted` is no longer consulted after the EOF
		// fallback — the no-op at function exit covers it.
		_ = inserted
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
