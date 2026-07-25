package netns

// Single source of truth for the tenant-egress denylist. Spec §11 +
// ADR-023 (v6 family split) + ADR-034 (6to4 + Teredo). Three
// consumers — the per-netns renderer (pkg/netns/config.go), the host
// renderer (pkg/netns/policy.go), and the OCI puller
// (pkg/oci/egress.go) — all read from NewDefaultDenySet() so the
// firewall rules and the user-space check can never drift apart.
//
// Renaming a field on HostPolicy / inlining new CIDRs in
// NftCommands() / adding a deny to deniedCIDRv4 in oci/egress.go is
// how this code base silently dropped a deny line in the past
// (issue #146). The DenySet type makes "the deny list" a thing
// you import; add a new CIDR once, three places update.

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// Family identifies which nft family an entry applies to. The
// per-netns forward chain is split into `ip faas` and `ip6 faas`
// (ADR-023); the host forward chain is a single `table inet faas`
// chain that uses `ip daddr` / `ip6 daddr` directly. The renderer
// picks the right nft keyword from this tag.
type Family int

const (
	FamilyV4 Family = iota
	FamilyV6
)

func (f Family) String() string {
	switch f {
	case FamilyV4:
		return "ip"
	case FamilyV6:
		return "ip6"
	default:
		return fmt.Sprintf("Family(%d)", int(f))
	}
}

// DenyEntry is a single CIDR entry on the denylist. SourceADR +
// Comment make the provenance machine-readable so a future "list
// every deny line" operator tool can render the table from
// introspection rather than a hand-maintained doc.
type DenyEntry struct {
	Family    Family
	Prefix    netip.Prefix
	SourceADR string
	Comment   string
}

// DenySet is the typed denylist. The four slice fields are the
// canonical allowlist-of-deny CIDRs / ports / counters every
// renderer reads. Entries is the metadata-rich view used by
// operator tooling and the regression net; the typed slices are
// derived from Entries so they can't drift apart.
type DenySet struct {
	// V4DenyCIDRs is the IPv4 egress denylist (spec §11).
	V4DenyCIDRs []netip.Prefix
	// V6DenyCIDRs is the IPv6 egress denylist (ADR-023 + ADR-034).
	V6DenyCIDRs []netip.Prefix
	// SMTPPorts is the egress TCP port denylist (spec §11):
	// 25, 465, 587. Spam = Hetzner abuse desk = existential
	// (spec §7 founding doc R6).
	SMTPPorts []uint16
	// ConntrackCap is the §7 per-instance conntrack cap (default
	// 4096). Renderers may consult this for telemetry / dashboard
	// exposition but it does NOT participate in the deny-line argv
	// (the cap is its own `ct count over N drop` rule).
	ConntrackCap uint32
	// Entries is the provenance-bearing view. Length == len(V4DenyCIDRs)
	// + len(V6DenyCIDRs); same data, sorted by family then CIDR.
	Entries []DenyEntry
}

// NewDefaultDenySet returns the platform-wide default denylist.
// This is the single function to edit when adding a new CIDR —
// every consumer (per-netns, host, oci) reads from it.
//
// Provenance: every entry names the ADR or RFC that sourced it.
// "spec" entries trace to spec §11 + spec §7 founding doc;
// "ADR-NNN" entries are platform decisions. Do not edit without
// an ADR — see the ADR-031 + ADR-033 precedent for "why each
// line is here".
func NewDefaultDenySet() DenySet {
	entries := []DenyEntry{
		// IPv4 — RFC1918 + link-local/metadata + CGN (spec §11).
		{FamilyV4, netip.MustParsePrefix("10.0.0.0/8"), "spec-§11", "RFC1918 — private network"},
		{FamilyV4, netip.MustParsePrefix("172.16.0.0/12"), "spec-§11", "RFC1918 — private network"},
		{FamilyV4, netip.MustParsePrefix("192.168.0.0/16"), "spec-§11", "RFC1918 — private network"},
		{FamilyV4, netip.MustParsePrefix("169.254.0.0/16"), "spec-§11", "link-local; 169.254.169.254 = cloud metadata IMDS"},
		{FamilyV4, netip.MustParsePrefix("100.64.0.0/10"), "RFC6598", "carrier-grade NAT"},

		// IPv6 — link-local + ULA + multicast + loopback + unspecified
		// (ADR-023 + ADR-034).
		{FamilyV6, netip.MustParsePrefix("fe80::/10"), "ADR-023", "IPv6 link-local; neighbor-table exposure to guests"},
		{FamilyV6, netip.MustParsePrefix("fc00::/7"), "ADR-023", "IPv6 ULA (RFC4193); control-plane lateral movement"},
		{FamilyV6, netip.MustParsePrefix("ff00::/8"), "ADR-023", "IPv6 multicast; no use case in this model"},
		{FamilyV6, netip.MustParsePrefix("::1/128"), "ADR-023", "IPv6 loopback"},
		{FamilyV6, netip.MustParsePrefix("::/128"), "ADR-023", "IPv6 unspecified; misconfigured or malicious"},
		{FamilyV6, netip.MustParsePrefix("2002::/16"), "ADR-034", "6to4 (RFC3056); tunnels IPv6 over IPv4 — lateral movement into 10/8 etc."},
		{FamilyV6, netip.MustParsePrefix("2001::/32"), "ADR-034", "Teredo (RFC4380); tunnels IPv6 over UDP/3544 — same lateral-movement risk as 6to4"},
	}

	// Sort Entries by family then prefix for deterministic ordering
	// across renderers + tests.
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Family != entries[j].Family {
			return entries[i].Family < entries[j].Family
		}
		return entries[i].Prefix.Addr().Less(entries[j].Prefix.Addr())
	})

	d := DenySet{
		SMTPPorts:    []uint16{25, 465, 587},
		ConntrackCap: 4096,
		Entries:      entries,
	}
	for _, e := range entries {
		switch e.Family {
		case FamilyV4:
			d.V4DenyCIDRs = append(d.V4DenyCIDRs, e.Prefix)
		case FamilyV6:
			d.V6DenyCIDRs = append(d.V6DenyCIDRs, e.Prefix)
		}
	}
	return d
}

// V4CommaSet returns V4DenyCIDRs joined with `,` (the modern-nft
// CIDR-set syntax gate — see memory `nft-cidr-set-comma-required`).
// Used by both per-netns and host renderers so the renderer
// surface stays one helper, not three.
func (d DenySet) V4CommaSet() string {
	parts := make([]string, len(d.V4DenyCIDRs))
	for i, p := range d.V4DenyCIDRs {
		parts[i] = p.String()
	}
	return strings.Join(parts, ",")
}

// V6CommaSet is the IPv6 sibling of V4CommaSet.
func (d DenySet) V6CommaSet() string {
	parts := make([]string, len(d.V6DenyCIDRs))
	for i, p := range d.V6DenyCIDRs {
		parts[i] = p.String()
	}
	return strings.Join(parts, ",")
}

// SMTPPortsCommaSet renders SMTPPorts as a comma-joined uint16 list
// for the nft `tcp dport { … } drop` set syntax. Mirrors the
// HostPolicy.joinInts helper but with uint16 (SMTPPorts is the
// typed slice; the int slice on HostPolicy is the legacy surface
// the helper consumed).
func (d DenySet) SMTPPortsCommaSet() string {
	parts := make([]string, len(d.SMTPPorts))
	for i, p := range d.SMTPPorts {
		parts[i] = fmt.Sprintf("%d", p)
	}
	return strings.Join(parts, ",")
}
