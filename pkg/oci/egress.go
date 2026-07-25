package oci

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"

	"github.com/onebox-faas/faas/pkg/netns"
)

// Egress policy for the OCI puller (spec §11). Tenant egress is fenced by
// nftables; the puller's own HTTP client applies the same denylist in
// user-space so a misconfigured firewall never lets a public pull reach an
// internal address. The list is the conservative union of:
//
//   - RFC1918 (10/8, 172.16/12, 192.168/16)
//   - loopback (127/8)
//   - link-local (169.254/16) — covers the cloud metadata service (IMDS)
//   - IPv6 unique-local (fc00::/7) and link-local (fe80::/10)
//   - IPv4/IPv6 unspecified + multicast
//
// We DENY every denied range and only allow addresses in the public ranges.
// The transport refuses both DNS lookups that resolve into a denied range
// AND direct IPs in a denied range — closes the DNS-rebinding hole that a
// naive IP allowlist leaves open.
//
// The base list is the shared netns.DefaultDenySet so the user-space
// check and the firewall rules can't drift apart (PR-A, ADR-034).
// OCI adds a small set of client-only entries that the host firewall
// doesn't need (loopback / 0.0.0.0/8 / IETF-assigned / benchmarking /
// reserved) because the puller runs out of init — it's the process that
// happens to call into the OCI registry, and a misconfigured firewall
// plus a pull to a loopback is exactly the regression the user-space
// check is meant to catch. The OCI-only entries are listed in
// ociOnlyDenyCIDRs and union'd at dial time.
// ociOnlyDenyCIDRsV4 are ranges the OCI puller denies in addition
// to the shared netns.DefaultDenySet. The host firewall already
// covers RFC1918 / link-local / metadata via the per-netns
// chain; these are the client-side hardening extras (PR-D).
//
// Typed as []netns.DenyEntry so each entry carries the same
// ADR-backed provenance the shared catalog uses. The SourceADR
// pin is ADR-034 (the PR-A slice that introduced the typed
// catalog); the rationale follows the OCI-only rationale that
// lived as inline comments before the refactor. These entries
// are deliberately NOT in netns.NewDefaultDenySet() — they are
// process-level hardening, not platform-wide policy, and the
// host firewall / per-netns renderer do not need them.
// OCIOnlyDenyCIDRsV4 returns a copy of the OCI-only client-hardening
// entries as provenance-bearing netns.DenyEntry records. Exported
// (PR-D feedback) so cmd/denylist-md can consume the typed slice
// directly instead of maintaining a duplicate literal table — the
// previous shape had a silent drift surface (a future edit to
// ociOnlyDenyCIDRsV4 below would not auto-update the generated
// docs/denylist.md). The accessor returns a copy so the caller
// cannot mutate the runtime union. Read-only contract.
func OCIOnlyDenyCIDRsV4() []netns.DenyEntry {
	out := make([]netns.DenyEntry, len(ociOnlyDenyCIDRsV4))
	copy(out, ociOnlyDenyCIDRsV4)
	return out
}

var ociOnlyDenyCIDRsV4 = []netns.DenyEntry{
	{
		Family:    netns.FamilyV4,
		Prefix:    netip.MustParsePrefix("0.0.0.0/8"),
		SourceADR: "ADR-034",
		Comment:   "unspecified IPv4 source range (defence-in-depth)",
	},
	{
		Family:    netns.FamilyV4,
		Prefix:    netip.MustParsePrefix("127.0.0.0/8"),
		SourceADR: "ADR-034",
		Comment:   "loopback range; OCI puller runs outside tenant netns",
	},
	{
		Family:    netns.FamilyV4,
		Prefix:    netip.MustParsePrefix("192.0.0.0/24"),
		SourceADR: "ADR-034",
		Comment:   "IETF protocol assignments",
	},
	{
		Family:    netns.FamilyV4,
		Prefix:    netip.MustParsePrefix("198.18.0.0/15"),
		SourceADR: "ADR-034",
		Comment:   "benchmarking range",
	},
	{
		Family:    netns.FamilyV4,
		Prefix:    netip.MustParsePrefix("240.0.0.0/4"),
		SourceADR: "ADR-034",
		Comment:   "reserved IPv4 range",
	},
}

// deniedCIDRs builds the OCI deny set as the union of the shared
// netns.DefaultDenySet + the OCI-only extras above. Built once at
// init; ipAllowed reads from it on every dial. The shared set has
// been growing since PR-A (ADR-034 added 6to4 + Teredo) — a future
// editorial pass will decide whether the OCI-only entries deserve
// to land on the host side too; today the union is the cheapest
// contract that closes the regression.
var (
	deniedCIDRv4 = func() []netip.Prefix {
		base := netns.NewDefaultDenySet()
		out := make([]netip.Prefix, 0, len(base.V4DenyCIDRs)+len(ociOnlyDenyCIDRsV4))
		out = append(out, base.V4DenyCIDRs...)
		// OCI-only entries are typed netns.DenyEntry (PR-D); the
		// enforcement path needs raw prefixes so we project Prefix
		// out at union time. Provenance (SourceADR, Comment) is
		// preserved on the typed slice and surfaces in the
		// generated docs/denylist.md; the runtime deny check is
		// unchanged.
		for _, e := range ociOnlyDenyCIDRsV4 {
			out = append(out, e.Prefix)
		}
		return out
	}()
	deniedCIDRv6 = func() []netip.Prefix {
		base := netns.NewDefaultDenySet()
		return append([]netip.Prefix{}, base.V6DenyCIDRs...)
	}()
)

// EgressDialContext returns a DialContext that rejects every address that
// resolves into a denied range. It is the transport that cmd/imaged and
// pkg/builderd plug into the OCI puller.
//
// The check happens AFTER DNS resolution so a hostname that resolves to a
// denied IP is refused (this is the same class of attack the firewall
// denylist on the box itself tries to catch — the user-space check is the
// belt-and-braces duplicate the financial model relies on: a bug in either
// layer still leaves the other holding).
func EgressDialContext(parent *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if parent == nil {
		parent = &net.Dialer{}
	}
	resolver := parent.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("oci: egress: bad addr %q: %w", addr, err)
		}
		ips, err := resolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("oci: egress: resolve %s: %w", host, err)
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("oci: egress: no addresses for %s", host)
		}
		// Reject the whole dial if ANY returned address is denied — a hostname
		// that resolves to both a public and a private IP would otherwise be
		// race-able, and the §11 policy is the conservative deny.
		for _, ipa := range ips {
			addr, ok := netip.AddrFromSlice(ipa.IP)
			if !ok {
				return nil, fmt.Errorf("oci: egress: unparseable address for %s", host)
			}
			addr = addr.Unmap()
			if !ipAllowed(addr) {
				// ADR-021: lift the egress-denial failure mode to a
				// sentinel that pkg/api.SentinelToCode maps to the
				// RFC 7807 CodeImageEgressDenied (security-class
				// signal, 403). The legacy ErrEgressDenied sentinel
				// below is wrapped inside this one for backwards
				// compat — pkg/oci consumers that already check
				// errors.Is(err, ErrEgressDenied) continue to work,
				// and pkg/api.SentinelToCode picks up the new
				// canonical sentinel.
				return nil, fmt.Errorf("%w: %w: address %s (%s)",
					ErrImageEgressDenied, ErrEgressDenied, host, addr)
			}
		}
		// Dial the first public IP explicitly to defeat DNS rebinding across
		// the resolution/dial gap.
		return parent.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
	}
}

// NewEgressHTTPClient returns an *http.Client that uses EgressDialContext as
// its transport dialer. Callers who already have an http.Client with options
// (proxy, timeouts) can build one with http.DefaultTransport and override
// only the dial hook:
//
//	tr := &http.Transport{DialContext: oci.EgressDialContext(nil)}
//	hc := &http.Client{Transport: tr, Timeout: 30*time.Second}
func NewEgressHTTPClient() *http.Client {
	tr := &http.Transport{
		DialContext: EgressDialContext(nil),
	}
	return &http.Client{Transport: tr}
}

// ipAllowed reports whether ip is in a publicly routable range. It is the
// single place the egress policy is enforced; add a new denied range to
// deniedCIDRv4 / deniedCIDRv6 and the test in egress_test.go picks it up.
func ipAllowed(ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	if ip.IsLoopback() || ip.IsMulticast() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() {
		return false
	}
	var denied []netip.Prefix
	if ip.Is4() || ip.Is4In6() {
		denied = deniedCIDRv4
	} else {
		denied = deniedCIDRv6
	}
	for _, p := range denied {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// EgressIPAllowed reports whether addr is permitted by the OCI
// puller's egress policy. It is the testable mirror of the
// internal ipAllowed predicate (PR-D) and exists so external
// packages — notably pkg/netns_test — can assert the shared
// catalog is consumed by the OCI dialer without exposing the
// internal slices or the resolver/dial plumbing. Adding a new
// catalog entry to netns.NewDefaultDenySet() is automatically
// picked up here because deniedCIDRv4 / deniedCIDRv6 are built
// from that source at init.
func EgressIPAllowed(addr netip.Addr) bool { return ipAllowed(addr) }

// ErrEgressDenied is returned (wrapped) when a dial target violates the
// §11 policy. Callers can errors.Is against it.
var ErrEgressDenied = errors.New("oci: egress denied")
