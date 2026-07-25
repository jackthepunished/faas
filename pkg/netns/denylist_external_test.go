// Package netns_test — external cross-renderer invariant test (PR-D).
//
// This file lives in the external test package so it can import
// pkg/oci without forming a cycle (pkg/oci already imports
// pkg/netns). The internal denylist_test.go covers provenance,
// typed-slice coherence, and the per-entry shape of NewDefaultDenySet;
// this file covers the harder contract: every entry in the shared
// catalog must be DENIED by all three enforcement surfaces.
//
// Three consumers:
//
//   - Per-netns nftables renderer: Config.NftCommands() —
//     table "ip faas" / "ip6 faas", chain "forward".
//   - Host nftables renderer: DefaultHostPolicy.Render() —
//     table "inet faas", chain "forward".
//   - OCI puller dialer: oci.EgressIPAllowed — the exported
//     wrapper added in PR-D for exactly this purpose.
//
// The test enumerates NewDefaultDenySet().Entries (the canonical,
// provenance-bearing view) and asserts that each entry's CIDR is
// present in all three sinks. The OCI-only v4 extras in
// pkg/oci/egress.go are tested in pkg/oci/egress_test.go — they
// are NOT in the shared catalog and must NOT be in this loop.
//
// The test is intentionally pure: no os/exec, no nft. It is the
// non-metal proof that the spec §11 contract holds end-to-end.
package netns_test

import (
	"net/netip"
	"strconv"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/netns"
	"github.com/onebox-faas/faas/pkg/oci"
)

// extractChainBody returns the body of the chain named `chain`
// inside `rendered`. Mirrors the brace-depth-walking helper used
// by the internal policy_test (memory `pkg-netns-brace-depth-extractchain`).
// Copied here because we are in package netns_test and cannot reach
// unexported test helpers. Anchor on " <name> {" preceded by any
// amount of indent / table-prefix so we work for both
// "  chain forward {" (host) and " chain ip faas forward {"
// (per-netns argv).
func extractChainBody(t *testing.T, rendered, chain string) string {
	t.Helper()
	openTag := " " + chain + " {"
	start := strings.Index(rendered, openTag)
	if start < 0 {
		t.Fatalf("chain %q not found in rendered ruleset", chain)
	}
	body := rendered[start+len(openTag):]
	depth := 1
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return body[:i]
			}
		}
	}
	t.Fatalf("chain %q has no depth-zero closing brace", chain)
	return ""
}

// flattenArgv joins each [][]string argv row with spaces and
// concatenates rows with newlines. Mirrors pkg/netns/config_test.go::flatten.
func flattenArgv(cmds [][]string) string {
	var b strings.Builder
	for _, c := range cmds {
		b.WriteString(strings.Join(c, " "))
		b.WriteString("\n")
	}
	return b.String()
}

// familyDaddrKeyword returns the nft daddr keyword for a family
// ("ip" or "ip6"). Hard-coded so a Family rename surfaces as a
// clear test failure instead of a silent semantic shift.
func familyDaddrKeyword(f netns.Family) string {
	if f == netns.FamilyV6 {
		return "ip6 daddr"
	}
	return "ip daddr"
}

// sampleAddrInPrefix returns a deterministic address inside the
// prefix for OCI predicate probing. Result is masked to guarantee
// it's inside the prefix even for boundary cases like 127.0.0.0/8.
func sampleAddrInPrefix(p netip.Prefix) netip.Addr {
	addr := p.Addr()
	bits := p.Bits()
	if bits == 0 {
		if addr.Is4() {
			return netip.MustParseAddr("1.0.0.0")
		}
		return netip.MustParseAddr("::1")
	}
	next := addr.As16()
	if bits < 128 {
		hostBytes := (128 - bits) / 8
		hostBits := (128 - bits) % 8
		if hostBits > 0 {
			next[hostBytes] |= 1 << (8 - hostBits - 1)
		} else if hostBytes+1 <= 15 {
			next[hostBytes] |= 0x80
		}
	}
	a, ok := netip.AddrFromSlice(next[:])
	if !ok {
		return addr
	}
	a = a.Unmap()
	masked := netip.PrefixFrom(a, bits).Masked()
	return masked.Addr()
}

// rowHasCIDRDeny scans the flattened argv for a rule row of the shape
// `<daddrKW> { ... <cidr> ... } drop`. The argv is row-delimited; each
// row is a full nft call. A regression that drops the CIDR, swaps
// the family tag, or short-circuits to `accept` instead of `drop`
// surfaces here.
func rowHasCIDRDeny(argv, daddrKW, cidr string) bool {
	for _, row := range strings.Split(argv, "\n") {
		if !strings.Contains(row, daddrKW) {
			continue
		}
		if !strings.Contains(row, cidr) {
			continue
		}
		if !strings.Contains(row, "} drop") {
			continue
		}
		return true
	}
	return false
}

// rowHasSMTPPort scans the flattened argv for the SMTP-port rule row
// `tcp dport { 25,465,587 } drop` (or a sibling row containing the
// specific port token).
func rowHasSMTPPort(argv, port string) bool {
	for _, row := range strings.Split(argv, "\n") {
		if !strings.Contains(row, "tcp dport") {
			continue
		}
		if !strings.Contains(row, port) {
			continue
		}
		if !strings.Contains(row, "} drop") {
			continue
		}
		return true
	}
	return false
}

// TestAllThreeConsumersAgreeOnDenySet (PR-D) is the non-metal
// cross-renderer invariant test. Every entry in
// NewDefaultDenySet() must be denied by:
//
//	(a) per-netns Config.NftCommands() — the `ip daddr { … } drop`
//	    or `ip6 daddr { … } drop` rule row in the per-netns forward chain.
//	(b) host DefaultHostPolicy.Render() — same line in the host
//	    forward chain body.
//	(c) oci.EgressIPAllowed(sample) == false — the user-space dial
//	    check used by the OCI puller.
//
// A regression that drops one entry from any sink — or removes it
// from NewDefaultDenySet() — surfaces here as a t.Errorf with the
// specific entry, family, and sink that disagreed.
func TestAllThreeConsumersAgreeOnDenySet(t *testing.T) {
	ds := netns.NewDefaultDenySet()
	if len(ds.Entries) == 0 {
		t.Fatal("NewDefaultDenySet() returned empty Entries; cannot enforce the invariant")
	}

	cfg := netns.NewConfig("instance", "faas-ns", "veth-host", "veth-peer", netip.MustParseAddr("10.100.0.1"))
	cfg.Tap = "tap0"
	cfg.EgressAllowlist = nil
	cfg.ConntrackCap = 4096
	perNetnsArgv := flattenArgv(cfg.NftCommands())

	hostRender := netns.DefaultHostPolicy.Render()
	hostFwd := extractChainBody(t, hostRender, "forward")

	for i, e := range ds.Entries {
		cidr := e.Prefix.String()
		daddrKW := familyDaddrKeyword(e.Family)
		daddrNeedle := daddrKW + " { "

		// Per-netns sink.
		if !rowHasCIDRDeny(perNetnsArgv, daddrKW, cidr) {
			t.Errorf("entries[%d] (%s) missing from per-netns %q deny argv row", i, cidr, daddrKW)
		}

		// Host sink.
		idx := strings.Index(hostFwd, daddrNeedle)
		if idx < 0 {
			t.Errorf("entries[%d] (%s, %s) host forward chain missing %q deny line",
				i, cidr, daddrKW, daddrKW)
			continue
		}
		end := strings.Index(hostFwd[idx:], " } drop")
		if end < 0 {
			t.Errorf("entries[%d] (%s) host %q line has no `} drop` closer", i, cidr, daddrKW)
			continue
		}
		if !strings.Contains(hostFwd[idx:idx+end], cidr) {
			t.Errorf("entries[%d] (%s) missing from host %q deny line", i, cidr, daddrKW)
		}

		// OCI sink.
		sample := sampleAddrInPrefix(e.Prefix)
		if oci.EgressIPAllowed(sample) {
			t.Errorf("entries[%d] (%s) sample %s ALLOWED by oci.EgressIPAllowed, want denied",
				i, cidr, sample)
		}
	}
}

// TestAllThreeConsumersAgreeOnSMTPPorts (PR-D) — the SMTP deny
// set is a separate typed slice on DenySet (not Entries), so the
// CIDR loop above doesn't cover it. Spec §11 requires 25/465/587
// to be denied by the renderer sinks; OCI has no SMTP-port concept
// so the assertion is renderer-only.
func TestAllThreeConsumersAgreeOnSMTPPorts(t *testing.T) {
	ds := netns.NewDefaultDenySet()
	wantPorts := map[uint16]bool{25: false, 465: false, 587: false}
	for _, p := range ds.SMTPPorts {
		if _, ok := wantPorts[p]; ok {
			wantPorts[p] = true
		}
	}
	for port, present := range wantPorts {
		if !present {
			t.Errorf("NewDefaultDenySet().SMTPPorts missing %d (spec §11)", port)
		}
	}

	cfg := netns.NewConfig("instance", "faas-ns", "veth-host", "veth-peer", netip.MustParseAddr("10.100.0.1"))
	cfg.Tap = "tap0"
	cfg.ConntrackCap = 4096
	perNetnsArgv := flattenArgv(cfg.NftCommands())
	hostFwd := extractChainBody(t, netns.DefaultHostPolicy.Render(), "forward")

	for _, p := range ds.SMTPPorts {
		needle := strconv.Itoa(int(p))
		if !rowHasSMTPPort(perNetnsArgv, needle) {
			t.Errorf("SMTP port %s missing from per-netns argv row", needle)
		}
		if !strings.Contains(hostFwd, needle) {
			t.Errorf("SMTP port %s missing from host forward chain", needle)
		}
	}
}
