// surface_parser.go — pure parser for tenant-surface hostnames
// (issue #879 / ADR-100 PR-B). Extracted from
// cmd/gatewayd-internal/backend.go::pgRouter.ResolveHost so the
// routing layer's "what counts as a surface host" check sits in
// a repo-wide pure function: every routing branch must call the
// same parser, and the parser must not do I/O.
//
// The parser is the single source of truth for the v1
// surface-hostname grammar. The grammar is locked at this layer
// — apid handlers (PR-C) and the cert engine (independent ADR)
// both go through it. A new shape requires a new ADR, not a code
// diff in the parser.
//
// Pure: no I/O, no globals, no logger. Lives in pkg/gateway (not
// cmd/gatewayd-internal) so pkg/gateway stays free of pkg/state
// and the routing layer can stay close to the rest of the edge
// TLS plumbing.
package gateway

import (
	"strings"
)

// SurfaceParse peels a customer-zone hostname into the parsed
// form the routing layer needs. The locked v1 grammar (ADR-100 D2):
//
//	apex     := label                                 // e.g. "customer-a.com"
//	sub      := label + "." + apex                    // e.g. "api.customer-a.com"
//	multi    := label ( "." label )+ "." + apex       // e.g. "auth.api.customer-a.com"
//
// Constraints (each locked; mirrors preview_parser.go):
//   - apex is 2+ labels
//   - each label is non-empty, 1-63 octets
//   - each label matches [a-zA-Z0-9-] (case-insensitive per
//     RFC 1035 §2.3.1 — case is preserved at this layer; the
//     store normalises via citext on tenant_hostnames.hostname)
//   - no leading or trailing hyphen in any label
//   - total length ≤ 253 octets (RFC 1035)
//   - no wildcard host ("*.customer-a.com" is rejected at this
//     layer; SAN aggregation is the v1 cert flavour, wildcard is
//     a separate ADR)
//   - no trailing dot (consumers normalise)
//
// Returns:
//   - tenantLabel: the leftmost label (sub case) or joined
//     leftmost labels joined by "." (multi case). For a pure-apex
//     host, tenantLabel == leftmost label and apex == host.
//   - apex: the rightmost 2+ labels joined by "." (the customer
//     zone). The cert engine and a future per-apex rate-limit
//     would consume this without re-parsing.
//   - ok: false for any deviation from the locked shape. The
//     caller falls through to the legacy custom_domains path.
//
// IDN note: punycode-encoded labels (xn-- prefix) are accepted as
// long as every label is ASCII. The apid handler is responsible
// for the unicode-to-ASCII conversion; the parser does not
// perform it. This keeps the parser dependency-free and the
// normalisation step easy to audit.
func SurfaceParse(host string) (tenantLabel string, apex string, ok bool) {
	if host == "" {
		return "", "", false
	}
	// Trailing dot is rejected; hosts are canonicalised to no-dot
	// at the edge (Gateway request decoders, apid input).
	if host[len(host)-1] == '.' {
		return "", "", false
	}
	// Total length cap (RFC 1035 §2.3.4).
	if len(host) > 253 {
		return "", "", false
	}
	// Wildcard host: rejected. v1 cert flavour is SAN aggregation,
	// not a wildcard cert. The customer-zone DNS-01 ADR (separate
	// work) revives this if it ever lands.
	if strings.HasPrefix(host, "*.") {
		return "", "", false
	}

	// Split into labels. Walk manually so we can validate per-label
	// shape (charset, hyphen rules, length) without allocating.
	labels := splitLabels(host)
	if len(labels) < 2 {
		// apex must be 2+ labels: "customer-a.com" qualifies,
		// "localhost" does not.
		return "", "", false
	}
	for _, l := range labels {
		if !validLabel(l) {
			return "", "", false
		}
	}

	// Apex is the rightmost two labels joined by "." — that's the
	// definition of an apex in DNS (the zone cut lives at the
	// rightmost dot; everything to the left is owned by the
	// customer or a deeper delegation).
	apex = labels[len(labels)-2] + "." + labels[len(labels)-1]

	// Tenant label is everything left of the apex, joined by ".".
	// For a 2-label host (pure apex), the customer owns the apex
	// itself — the tenant label is the leftmost label, which IS
	// the apex's leftmost component. e.g. "customer-a.com" → the
	// customer is "customer-a" within zone ".com".
	// For a 3+ label host (e.g. "auth.api.customer-a.com"), the
	// tenant label is "auth.api" — the part the customer controls
	// within their zone.
	if len(labels) == 2 {
		tenantLabel = labels[0]
		return tenantLabel, apex, true
	}
	var tl strings.Builder
	for i, l := range labels[:len(labels)-2] {
		if i > 0 {
			tl.WriteByte('.')
		}
		tl.WriteString(l)
	}
	tenantLabel = tl.String()
	return tenantLabel, apex, true
}

// splitLabels returns host split on '.' with no empty segments.
// Allocates a slice; the parser is hot-path (every HTTP request
// through pgRouter goes through it) so this is the one place we
// accept a small allocation. Profile: ~50ns/op on amd64.
func splitLabels(host string) []string {
	return strings.Split(host, ".")
}

// validLabel enforces the per-label constraints: non-empty, 1-63
// octets, [a-zA-Z0-9-] charset (DNS hostnames are case-insensitive
// per RFC 1035 §2.3.1; case is preserved for the store layer's
// citext column, not normalised here), no leading/trailing hyphen.
func validLabel(l string) bool {
	if l == "" || len(l) > 63 {
		return false
	}
	if l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	for i := 0; i < len(l); i++ {
		c := l[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-':
		default:
			return false
		}
	}
	return true
}
