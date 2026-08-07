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
//   1. `curl -X POST https://dns.hetzner.com/api/v1/records`
//      with the new A record body (UpsertRecord)
//   2. `curl -X DELETE https://dns.hetzner.com/api/v1/records/<id>`
//      with the old A record id (DeleteRecord)
//
// A staging operator can copy-paste the curls into their
// terminal; a CI pipeline can pipe stderr to a sidecar that
// calls the API on the operator's behalf.
//
// The provider is real — UpsertRecord and DeleteRecord return
// nil immediately so the orchestrator's drain protocol
// progresses. The metric bump is identical to the Hetzner
// path; the operator-runbook's escalation section covers the
// "I missed a curl in stderr" case.
package gateway

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// ManualDNSProvider implements DNSProvider by printing the
// required `curl` to a configured io.Writer (defaults to
// os.Stderr). The struct fields are unexported because callers
// should use NewManualDNSProvider.
type ManualDNSProvider struct {
	zone   string
	stderr io.Writer
	mu     sync.Mutex
}

// NewManualDNSProvider builds the manual fallback. The token
// is intentionally NOT consulted — the operator owns the curl
// execution; passing one in is a misconfiguration but harmless
// (the provider never sends it anywhere).
func NewManualDNSProvider(cfg DNSProviderConfig) (*ManualDNSProvider, error) {
	if cfg.Zone == "" {
		return nil, fmt.Errorf("manual dns: Zone required (e.g. 'example.com')")
	}
	stderr := cfg.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	return &ManualDNSProvider{
		zone:   cfg.Zone,
		stderr: stderr,
	}, nil
}

// UpsertRecord prints the required `curl` to stderr and
// returns nil. The `name` is the fully-qualified domain; the
// `value` is the leader's egress IP.
func (m *ManualDNSProvider) UpsertRecord(_ context.Context, name, value string) error {
	if name == "" || value == "" {
		return fmt.Errorf("manual dns: name and value required (got name=%q value=%q)", name, value)
	}
	body := fmt.Sprintf(`{"zone_id":"<ZONE_ID>","name":%q,"type":"A","value":%q,"ttl":60}`,
		name, value)
	// Use Hetzner DNS as the canonical example since
	// FAAS_DNS_PROVIDER=hetzner is the production path. A
	// staging operator on Route53 would substitute the
	// Route53 URL — covered in the runbook's pre-flight.
	curl := fmt.Sprintf(
		"# FAAS_DNS_PROVIDER=manual: UpsertRecord\n"+
			"# Get zone id first:\n"+
			"#   curl -H 'Auth-API-Token: $TOKEN' 'https://dns.hetzner.com/api/v1/zones?name=%s'\n"+
			"# Then create the A record (replace <ZONE_ID>):\n"+
			"curl -X POST 'https://dns.hetzner.com/api/v1/records' \\\n"+
			"  -H 'Auth-API-Token: $TOKEN' \\\n"+
			"  -H 'Content-Type: application/json' \\\n"+
			"  -d '%s'\n",
		m.zone, body)
	m.write(curl)
	return nil
}

// DeleteRecord prints the required `curl` to stderr and
// returns nil. The operator must look up the record id first
// (Hetzner's PUT-by-id requires it).
func (m *ManualDNSProvider) DeleteRecord(_ context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("manual dns: name required")
	}
	curl := fmt.Sprintf(
		"# FAAS_DNS_PROVIDER=manual: DeleteRecord\n"+
			"# List records to find the id:\n"+
			"#   curl -H 'Auth-API-Token: $TOKEN' 'https://dns.hetzner.com/api/v1/records?zone_id=<ZONE_ID>'\n"+
			"# Then delete (replace <RECORD_ID>):\n"+
			"curl -X DELETE 'https://dns.hetzner.com/api/v1/records/<RECORD_ID>' \\\n"+
			"  -H 'Auth-API-Token: $TOKEN'\n"+
			"# (deleting record for name=%q, zone=%s)\n",
		name, m.zone)
	m.write(curl)
	return nil
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