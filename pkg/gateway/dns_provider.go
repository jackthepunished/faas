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
//   - Cloudflare DNS — the first-class implementation. Caddy
//   - Cloudflare already terminates TLS for the same hostname
//     upstream of gatewayd-public, so the leader-election
//     A-record naturally lands on the same zone Cloudflare
//     serves — no separate DNS-01 plumbing required.
//   - manual — operator-managed fallback. Prints the required
//     `curl` to stderr so a staging operator can flip DNS by
//     hand. Same pattern as FAAS_STORAGE_CACHE_SERVE_STALE
//     (ADR-054 acceptance PR).
//
// Round 1 shipped a HetznerRecordProvider sibling to
// pkg/gateway/dns01_hetzner.go (the legacy ACME DNS-01 solver).
// Round 2 deletes it — production runs Cloudflare + Caddy, and
// the Hetzner plumbing was dead weight (see ADR-083 §3
// follow-up revision). The legacy ACME DNS-01 path stays until
// the ADR-024 PR-C sweep.
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
//	"cloudflare" → NewCloudflareRecordProvider (default if
//	               unset on production clusters)
//	"manual"     → NewManualDNSProvider (default if unset on
//	               staging / single-box dev)
//	anything else → error at boot; the operator must pick.
type DNSProviderConfig struct {
	// Zone is the DNS zone name (e.g. "example.com"). Cloudflare
	// resolves Zone → ZoneID internally (cached on the
	// receiver); the manual provider prints the lookup
	// instructions in the `curl` it emits.
	Zone string
	// SealedToken is the Cloudflare API token sealed via
	// pkg/secretbox.SealBytes(namespace="DNS_PROVIDER"). The
	// Cloudflare provider OpenBytes it; the manual provider
	// ignores it (no token needed).
	SealedToken []byte
	// APIURL overrides the Cloudflare API base for tests. The
	// manual provider ignores it. Empty string → production
	// default (https://api.cloudflare.com/client/v4).
	APIURL string
	// ProviderURL is the human-facing UI/API URL the manual
	// provider prints in the operator-facing `curl`. Empty
	// string → production default (Cloudflare API base,
	// https://api.cloudflare.com/client/v4). A staging operator
	// on Route53 would set
	// ProviderURL="https://console.aws.amazon.com/route53"
	// (review finding #5 round 1 — the previous hard-coded
	// Hetzner URL made the manual path useless for any
	// non-Hetzner provider; round 2 switched the default to
	// Cloudflare).
	ProviderURL string
	// Stderr is the io.Writer the manual provider prints to.
	// nil → os.Stderr (production default).
	Stderr interface{ Write([]byte) (int, error) }
}

// errDNSProviderUnknown is returned by NewDNSProvider when
// FAAS_DNS_PROVIDER is unset or unrecognised. The operator
// must pick one — the runbook's pre-flight section covers
// this.
var errDNSProviderUnknown = fmt.Errorf("dns provider: FAAS_DNS_PROVIDER must be one of {cloudflare, manual}")
