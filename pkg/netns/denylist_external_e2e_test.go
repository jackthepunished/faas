// Package netns_test — §11a cross-renderer triple-agreement tripwire.
//
// This file mirrors the PR-D TestAllThreeConsumersAgreeOnDenySet
// pattern (denylist_external_test.go) but is the §11a layer-down
// pin: every entry in NewDefaultDenySet() must be DENIED by all
// three enforcement surfaces, AND each entry's family tag must
// match the table family it lands in. The two previous pins
// (TestAllThreeConsumersAgreeOnDenySet + TestAllThreeConsumersAgreeOnSMTPPorts)
// already cover the per-sink deny presence, but do not assert
// the v4↔v6 split goes the right way on both renderers.
//
// The tripwire is intentionally not split per-sink: a single
// failure should localize cleanly to (cidr, family, sink) so
// the regression report points the fixer at the right line.
//
// External test package so it can import pkg/oci without
// forming a cycle (pkg/oci already imports pkg/netns). The
// internal denylist_committed_test.go covers per-renderer
// pin detail; this file covers the harder cross-renderer
// invariant.
package netns_test

import (
	"net/netip"
	"strconv"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/netns"
	"github.com/onebox-faas/faas/pkg/oci"
)

// sec11aSplitNftCommands is the §11a-flat equivalent of the
// splitNftCommands helper in denylist_committed_test.go. Both
// files live in `netns_test`; the helper is duplicated here
// rather than exported because the two files are siblings
// and the split is the §11a contract surface (changes to the
// split order — e.g. emitting v6 first — would invalidate both
// tests identically, which is exactly what the §11a pin wants).
func sec11aSplitNftCommands(t *testing.T, cfg netns.Config) (v4Argv, v6Argv string) {
	t.Helper()
	argv := cfg.NftCommands()
	var rows []string
	for _, c := range argv {
		rows = append(rows, strings.Join(c, " "))
	}
	flat := strings.Join(rows, "\n")

	v6Marker := "add table ip6 faas"
	idx := strings.Index(flat, v6Marker)
	if idx < 0 {
		t.Fatalf("per-netns argv missing %q — v6 split assumed by ADR-023 broke", v6Marker)
	}
	return flat[:idx], flat[idx:]
}

// findRowContaining scans the flattened argv for a row that
// contains `needle` and returns the row. Returns "" if no row
// matches — callers must check for the empty string when the
// presence of a row is the assertion (the §11a triple
// agreement uses the empty string as a sink-miss signal).
func findRowContaining(argv, needle string) string {
	for _, row := range strings.Split(argv, "\n") {
		if strings.Contains(row, needle) {
			return row
		}
	}
	return ""
}

// TestSec11a_CrossRendererTripleAgreement — every entry in
// NewDefaultDenySet() must be denied by:
//
//	(a) per-netns Config.NftCommands() — the per-CIDR deny
//	    row in the forward chain matching the entry's family.
//	(b) host DefaultHostPolicy.Render() — the per-CIDR deny
//	    line in the host forward chain matching the entry's
//	    family.
//	(c) oci.EgressIPAllowed(sample) == false — the user-space
//	    dial check used by the OCI puller.
//
// In addition, the test pins the SMTP port deny (25/465/587)
// across both renderers — OCI has no port concept so the SMTP
// assertion is renderer-only.
//
// A regression that drops one entry from any sink — or
// mis-families it — surfaces as a t.Errorf with the specific
// entry, family, and sink that disagreed.
//
// Runs on every-PR CI (no build tag, no root, no /dev/kvm).
// The authoritative cross-process kernel-attribute gate stays
// in pkg/fcvm/manager_metal_test.go::TestMetalGuestEgressToPublicViaMASQUERADE
// (//go:build metal).
func TestSec11a_CrossRendererTripleAgreement(t *testing.T) {
	ds := netns.NewDefaultDenySet()
	if len(ds.Entries) == 0 {
		t.Fatal("NewDefaultDenySet() returned empty Entries; cannot enforce the §11a invariant")
	}

	cfg := netns.NewConfig("instance", "faas-ns", "veth-host", "veth-peer", netip.MustParseAddr("10.100.0.2"))
	cfg.Tap = "tap0"
	cfg.EgressAllowlist = nil
	cfg.ConntrackCap = 4096
	perNetnsV4, perNetnsV6 := sec11aSplitNftCommands(t, cfg)
	hostRender := netns.DefaultHostPolicy.Render()

	for i, e := range ds.Entries {
		cidr := e.Prefix.String()
		family := e.Family.String() // "ip" or "ip6"

		// Per-netns sink: per-CIDR deny row, scoped to the
		// entry's family (the v4 / v6 split).
		perNetns := perNetnsV4
		if e.Family == netns.FamilyV6 {
			perNetns = perNetnsV6
		}
		denyNeedle := family + " daddr " + cidr
		row := findRowContaining(perNetns, denyNeedle)
		if row == "" {
			t.Errorf("entries[%d] (%s, %s) missing from per-netns argv", i, cidr, family)
		} else {
			// The per-CIDR rule must end in `drop` — a
			// regression that adds accept or dnat to the
			// rule would re-open the lateral-movement
			// path.
			trimmed := strings.TrimSpace(row)
			if !strings.HasSuffix(trimmed, "drop") {
				t.Errorf("entries[%d] (%s, %s) per-netns deny row does not end in `drop`: %q",
					i, cidr, family, trimmed)
			}
			// The per-CIDR rule must carry the iifname tap0
			// qualifier — the §11 graceful-isolation
			// invariant depends on the rule only matching
			// guest-originated traffic.
			if !strings.Contains(row, "iifname") {
				t.Errorf("entries[%d] (%s, %s) per-netns deny row missing iifname qualifier: %q",
					i, cidr, family, row)
			}
			if !strings.Contains(row, "tap0") {
				t.Errorf("entries[%d] (%s, %s) per-netns deny row missing tap0 token: %q",
					i, cidr, family, row)
			}
		}

		// Host sink: per-CIDR deny line in the unified
		// table inet faas forward chain.
		hostDenyNeedle := family + " daddr " + cidr
		idx := strings.Index(hostRender, hostDenyNeedle)
		if idx < 0 {
			t.Errorf("entries[%d] (%s, %s) missing from host render", i, cidr, family)
		} else {
			// Walk forward to the end of the line; the
			// line must end in `drop` and carry the
			// counter name from the catalog.
			line := hostRender[idx:]
			if nl := strings.Index(line, "\n"); nl >= 0 {
				line = line[:nl]
			}
			if !strings.HasSuffix(strings.TrimSpace(line), "drop") {
				t.Errorf("entries[%d] (%s, %s) host deny line does not end in `drop`: %q",
					i, cidr, family, line)
			}
			if !strings.Contains(line, "counter name \""+e.CounterName+"\"") {
				t.Errorf("entries[%d] (%s, %s) host deny line missing counter name %q: %q",
					i, cidr, family, e.CounterName, line)
			}
		}

		// OCI sink: the user-space dial check must deny
		// every entry. Use the same sample-addr-in-prefix
		// helper as the PR-D test for consistency.
		sample := sec11aSampleAddrInPrefix(e.Prefix)
		if oci.EgressIPAllowed(sample) {
			t.Errorf("entries[%d] (%s, %s) sample %s ALLOWED by oci.EgressIPAllowed, want denied",
				i, cidr, family, sample)
		}
	}

	// SMTP port deny across both renderers. OCI has no port
	// concept so the SMTP assertion is renderer-only.
	const smtpNeedle = "tcp dport { 25,465,587 } drop"
	if !strings.Contains(perNetnsV4, smtpNeedle) {
		t.Errorf("per-netns v4 argv missing SMTP deny %q", smtpNeedle)
	}
	if strings.Contains(perNetnsV6, "tcp dport") {
		t.Errorf("per-netns v6 argv has a tcp dport rule — SMTP deny must be v4-only per spec §11 + ADR-023")
	}
	if !strings.Contains(hostRender, smtpNeedle) {
		t.Errorf("host render missing SMTP deny %q", smtpNeedle)
	}
	for _, p := range ds.SMTPPorts {
		needle := strconv.Itoa(int(p))
		// Per-netns SMTP port must appear in the comma-only
		// form on the same row as the rest of the set. Anchor
		// the row search on the SMTP-row's `tcp dport { … }`
		// opening — a naive "tcp dport" search would match
		// the prerouting DNAT row's `tcp dport 8080 dnat` and
		// report a false negative.
		row := findRowContaining(perNetnsV4, "tcp dport {")
		if !strings.Contains(row, needle) {
			t.Errorf("per-netns SMTP row missing port %s: %q", needle, row)
		}
		// Host render uses the same comma-only form.
		if !strings.Contains(hostRender, needle) {
			t.Errorf("host render missing SMTP port %s", needle)
		}
	}
}

// sec11aSampleAddrInPrefix returns a deterministic address
// inside the prefix for OCI predicate probing. Duplicated
// from denylist_external_test.go::sampleAddrInPrefix rather
// than exported because:
//
//   - the helper is test-only
//   - exporting it would only be used by this §11a file
//   - the §11a pin is intentionally independent of the PR-D
//     pin (a regression in one should not silently hide behind
//     the other)
//
// The implementation is identical to the PR-D version; the
// comment is preserved verbatim. Result is masked to guarantee
// it's inside the prefix even for boundary cases like
// 127.0.0.0/8.
func sec11aSampleAddrInPrefix(p netip.Prefix) netip.Addr {
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
