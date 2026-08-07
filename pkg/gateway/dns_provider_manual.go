// Manual DNS provider for the Tier A8 active-passive HA
// topology (ADR-083). Operator-managed fallback: instead of
// calling a DNS provider API, prints the required `curl` to
// stderr so a staging operator can flip DNS by hand.
//
// Pattern precedent: FAAS_STORAGE_CACHE_SERVE_STALE (ADR-054
// acceptance PR) — opt-in to operator-managed behaviour via a
// single env var, with a printed-curls path for the human in
// the loop. Tier A8 uses the same shape:
//
//	FAAS_DNS_PROVIDER=manual
//
// → every leader transition prints two curls:
//  1. `curl -X POST <ProviderURL>/api/v1/records` with the
//     new A record body (UpsertRecord)
//  2. `curl -X DELETE <ProviderURL>/api/v1/records/<id>`
//     with the old A record id (DeleteRecord)
//
// A staging operator can copy-paste the curls into their
// terminal; a CI pipeline can pipe stderr to a sidecar that
// calls the API on the operator's behalf.
//
// Review finding #5: the manual path previously hard-coded
// `https://dns.hetzner.com/api/v1/...` regardless of
// cfg.ProviderURL. A Route53 or Cloudflare operator would
// copy-paste a Hetzner-shaped curl, get rejected, and the
// orchestrator would still bump `dns_flipped` — a lie. The
// ProviderURL field fixes that.
//
// Review finding #14: the manual path previously returned
// nil on UpsertRecord/DeleteRecord regardless of whether the
// operator actually flipped DNS, so the orchestrator bumped
// `dns_flipped` even when nothing happened. Both methods now
// return a sentinel error so the orchestrator's drain path
// enters the dns_stale branch and surfaces "operator must
// run the curl" in the audit log + dashboard.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// errManualDNSRequiresOperator is the sentinel the manual
// provider returns from UpsertRecord / DeleteRecord. The
// orchestrator catches it as a non-retryable failure (the
// operator has to act), increments
// activePassiveFailoversTotal{outcome="dns_stale"} (NOT
// "dns_flipped" — DNS was NOT flipped), and surfaces the curl
// the operator needs to run via the runbook's manual drain
// command.
var errManualDNSRequiresOperator = errors.New("manual dns: operator must run the printed curl to flip DNS")

// ManualDNSProvider implements DNSProvider by printing the
// required `curl` to a configured io.Writer (defaults to
// os.Stderr). The struct fields are unexported because callers
// should use NewManualDNSProvider.
type ManualDNSProvider struct {
	zone        string
	providerURL string
	stderr      io.Writer
	mu          sync.Mutex
}

// NewManualDNSProvider builds the manual fallback. The token
// is intentionally NOT consulted — the operator owns the curl
// execution; passing one in is a misconfiguration but harmless
// (the provider never sends it anywhere).
//
// providerURL is the UI/API base the operator faces. Empty
// string → Hetzner DNS console default
// (https://dns.hetzner.com); a Route53 operator would set
// it to https://console.aws.amazon.com/route53 (review
// finding #5).
func NewManualDNSProvider(cfg DNSProviderConfig) (*ManualDNSProvider, error) {
	if cfg.Zone == "" {
		return nil, fmt.Errorf("manual dns: Zone required (e.g. 'example.com')")
	}
	stderr := cfg.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	providerURL := cfg.ProviderURL
	if providerURL == "" {
		providerURL = "https://dns.hetzner.com"
	}
	return &ManualDNSProvider{
		zone:        cfg.Zone,
		providerURL: providerURL,
		stderr:      stderr,
	}, nil
}

// UpsertRecord prints the required `curl` to stderr and
// returns errManualDNSRequiresOperator. The `name` is the
// fully-qualified domain; the `value` is the leader's egress
// IP. Returning the sentinel error (review finding #14)
// prevents the orchestrator from bumping `dns_flipped` —
// DNS was NOT flipped; the operator has to run the curl.
func (m *ManualDNSProvider) UpsertRecord(_ context.Context, name, value string) error {
	if name == "" || value == "" {
		return fmt.Errorf("manual dns: name and value required (got name=%q value=%q)", name, value)
	}
	body := fmt.Sprintf(`{"zone_id":"<ZONE_ID>","name":%q,"type":"A","value":%q,"ttl":60}`,
		name, value)
	curl := fmt.Sprintf(
		"# FAAS_DNS_PROVIDER=manual: UpsertRecord\n"+
			"# ProviderURL: %s\n"+
			"# Get zone id first:\n"+
			"#   curl -H 'Auth-API-Token: $TOKEN' '%s/api/v1/zones?name=%s'\n"+
			"# Then create the A record (replace <ZONE_ID>):\n"+
			"curl -X POST '%s/api/v1/records' \\\n"+
			"  -H 'Auth-API-Token: $TOKEN' \\\n"+
			"  -H 'Content-Type: application/json' \\\n"+
			"  -d '%s'\n",
		m.providerURL, m.providerURL, m.zone, m.providerURL, body)
	m.write(curl)
	return errManualDNSRequiresOperator
}

// DeleteRecord prints the required `curl` to stderr and
// returns errManualDNSRequiresOperator. The operator must
// look up the record id first (Hetzner's PUT-by-id requires
// it). Returning the sentinel prevents the orchestrator from
// bumping `dns_flipped` (review finding #14).
func (m *ManualDNSProvider) DeleteRecord(_ context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("manual dns: name required")
	}
	curl := fmt.Sprintf(
		"# FAAS_DNS_PROVIDER=manual: DeleteRecord\n"+
			"# ProviderURL: %s\n"+
			"# List records to find the id:\n"+
			"#   curl -H 'Auth-API-Token: $TOKEN' '%s/api/v1/records?zone_id=<ZONE_ID>'\n"+
			"# Then delete (replace <RECORD_ID>):\n"+
			"curl -X DELETE '%s/api/v1/records/<RECORD_ID>' \\\n"+
			"  -H 'Auth-API-Token: $TOKEN'\n"+
			"# (deleting record for name=%q, zone=%s)\n",
		m.providerURL, m.providerURL, m.providerURL, name, m.zone)
	m.write(curl)
	return errManualDNSRequiresOperator
}

// write serialises concurrent stderr writes so two simultaneous
// UpsertRecord calls don't interleave their curls. The lock
// is non-blocking for the operator (stderr writes are cheap).
func (m *ManualDNSProvider) write(s string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Trailing newline is idempotent if the operator pipes
	// stderr through `tee` (each write is one drain cycle).
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	_, _ = io.WriteString(m.stderr, s)
}

// NewDNSProvider is the constructor the dns_handoff
// orchestrator uses. Today it selects between the Hetzner and
// manual implementations based on the FAAS_DNS_PROVIDER env
// var (read by the caller and passed in via cfg); the package
// holds no env-var surface of its own (mirrors
// FAAS_STORAGE_CACHE_SERVE_STALE pattern — ADR-054).
func NewDNSProvider(cfg DNSProviderConfig, provider string) (DNSProvider, error) {
	switch provider {
	case "hetzner":
		return NewHetznerRecordProvider(cfg)
	case "manual":
		return NewManualDNSProvider(cfg)
	case "":
		return nil, errDNSProviderUnknown
	default:
		return nil, fmt.Errorf("%w (got %q)", errDNSProviderUnknown, provider)
	}
}
