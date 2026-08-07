// Package gateway — DNS provider abstraction for the Tier A8
// active-passive HA topology (ADR-083 / §14 M8 row "Gate-A runbook
// (2nd box active-passive)").
//
// The leader `gatewayd-public` calls UpsertRecord when it wins
// an election (so the public DNS record points at the new
// leader's egress IP) and DeleteRecord when it loses (so traffic
// drains cleanly off the dying node, bounded by
// api.HADNSRecordStaleSeconds). Two implementations ship in
// tier A8:
//
//   - Hetzner DNS — the first-class implementation. Same Hetzner
//     DNS API the existing pkg/gateway/dns01_hetzner.go uses for
//     the ACME DNS-01 solver; differs only in the record type
//     (A/AAAA vs TXT) and the zone/record-name surface.
//   - manual — operator-managed fallback. Prints the required
//     `curl` to stderr so a staging operator can flip DNS by
//     hand. Same pattern as FAAS_STORAGE_CACHE_SERVE_STALE
//     (ADR-054 acceptance PR).
//
// The package holds no state of its own; every call goes through
// the interface so the dns_handoff orchestrator in
// cmd/gatewayd-public/dns_handoff.go doesn't have to special-case
// provider choice.
package gateway

import (
	"context"
	"fmt"
)

// DNSProvider is the minimal interface the dns_handoff
// orchestrator needs. Two methods: UpsertRecord (idempotent —
// the caller may invoke it on every leader election even if the
// record already points at this node) and DeleteRecord
// (idempotent — a no-op on a record that doesn't exist is fine).
//
// Implementations MUST:
//   - Honour ctx — the orchestrator's drain protocol bounds
//     DeleteRecord by api.HADNSRecordStaleSeconds (30s).
//   - Surface transient errors (network 5xx, rate limit 429) as
//     err; the orchestrator's retry loop handles exponential
//     backoff (1s → 2s → 4s, capped at 30s, 5 retries).
//   - Surface permanent errors (4xx not-404) as err; the
//     orchestrator increments
//     activePassiveFailoversTotal{outcome="dns_stale"} and
//     pages the operator.
type DNSProvider interface {
	// UpsertRecord sets the A/AAAA record for `name` to `value`.
	// The `name` is a fully-qualified domain (e.g.
	// "faas-node-a.example.com"). The `value` is the leader's
	// egress IP (IPv4 today; IPv6 deferred — see ADR-033). On
	// a record that already exists with the same value, the
	// call is a no-op (idempotent).
	UpsertRecord(ctx context.Context, name string, value string) error

	// DeleteRecord removes the A/AAAA record for `name`. On a
	// record that doesn't exist, the call is a no-op
	// (idempotent — a re-election cycle can call DeleteRecord
	// after DeleteRecord).
	DeleteRecord(ctx context.Context, name string) error
}

// DNSProviderConfig is the constructor input. The provider
// choice is gated on env `FAAS_DNS_PROVIDER`:
//
//	"hetzner"  → NewHetznerDNSProvider (default if unset on
//	             production clusters)
//	"manual"   → NewManualDNSProvider (default if unset on
//	             staging / single-box dev)
//	anything else → error at boot; the operator must pick.
type DNSProviderConfig struct {
	// Zone is the DNS zone name (e.g. "example.com"). Hetzner
	// resolves Zone → ZoneID internally; the manual provider
	// prints it in the `curl` it emits.
	Zone string
	// SealedToken is the Hetzner DNS API token sealed via
	// pkg/secretbox.SealBytes(namespace="DNS_PROVIDER"). The
	// Hetzner provider OpenBytes it; the manual provider
	// ignores it (no token needed).
	SealedToken []byte
	// APIURL overrides the Hetzner DNS API base for tests. The
	// manual provider ignores it. Empty string → production
	// default (https://dns.hetzner.com/api/v1).
	APIURL string
	// Stderr is the io.Writer the manual provider prints to.
	// nil → os.Stderr (production default).
	Stderr interface{ Write([]byte) (int, error) }
}

// errDNSProviderUnknown is returned by NewDNSProvider when
// FAAS_DNS_PROVIDER is unset or unrecognised. The operator
// must pick one — the runbook's pre-flight section covers
// this.
var errDNSProviderUnknown = fmt.Errorf("dns provider: FAAS_DNS_PROVIDER must be one of {hetzner, manual}")