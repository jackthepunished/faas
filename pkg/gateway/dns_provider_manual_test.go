// Tests for the manual DNS provider (Tier A8 / ADR-083).
// Round 2: the curl shape is now Cloudflare-shaped (round 1
// was Hetzner-shaped — production runs Cloudflare + Caddy, not
// Hetzner). The CloudflareRecordProvider gets full httptest
// coverage in dns_provider_cloudflare_test.go.

package gateway

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// captureStderr is a goroutine-safe io.Writer for the manual
// provider's stderr sink.
type captureStderr struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *captureStderr) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *captureStderr) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// Review finding #14: UpsertRecord returns the sentinel error so
// the orchestrator enters the dns_stale branch (and does NOT
// bump dns_flipped).
func TestManualDNSProvider_UpsertRecord_ReturnsSentinel(t *testing.T) {
	cap := &captureStderr{}
	p, err := NewManualDNSProvider(DNSProviderConfig{
		Zone:   "example.com",
		Stderr: cap,
	})
	if err != nil {
		t.Fatalf("NewManualDNSProvider: %v", err)
	}
	err = p.UpsertRecord(context.Background(), "faas-node-a.example.com", "1.2.3.4")
	if !errors.Is(err, errManualDNSRequiresOperator) {
		t.Errorf("UpsertRecord err = %v, want errManualDNSRequiresOperator", err)
	}
	out := cap.String()
	// Round 2: Cloudflare-shaped curl. The old Hetzner-shaped
	// curl (`POST 'https://dns.hetzner.com/api/v1/records'`) is
	// gone — it was useless for a Cloudflare operator.
	if !strings.Contains(out, "POST 'https://api.cloudflare.com/client/v4/zones/<ZONE_ID>/dns_records'") {
		t.Errorf("stderr missing Cloudflare-shaped POST:\n%s", out)
	}
	if !strings.Contains(out, "Authorization: Bearer $CF_API_TOKEN") {
		t.Errorf("stderr missing Bearer auth header:\n%s", out)
	}
	if !strings.Contains(out, `"type":"A"`) {
		t.Errorf("stderr missing A-record body:\n%s", out)
	}
	if !strings.Contains(out, "faas-node-a.example.com") {
		t.Errorf("stderr missing record name:\n%s", out)
	}
	if !strings.Contains(out, "example.com") {
		t.Errorf("stderr missing zone:\n%s", out)
	}
}

func TestManualDNSProvider_DeleteRecord_ReturnsSentinel(t *testing.T) {
	cap := &captureStderr{}
	p, err := NewManualDNSProvider(DNSProviderConfig{
		Zone:   "example.com",
		Stderr: cap,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = p.DeleteRecord(context.Background(), "faas-node-a.example.com")
	if !errors.Is(err, errManualDNSRequiresOperator) {
		t.Errorf("DeleteRecord err = %v, want errManualDNSRequiresOperator", err)
	}
	out := cap.String()
	if !strings.Contains(out, "DELETE") {
		t.Errorf("stderr missing DELETE verb:\n%s", out)
	}
	if !strings.Contains(out, "/zones/<ZONE_ID>/dns_records/<RECORD_ID>") {
		t.Errorf("stderr missing Cloudflare-shaped DELETE path:\n%s", out)
	}
}

// ProviderURL override lets an operator on Route53 /
// DigitalOcean DNS / etc. produce a curl that points at their
// console. Round 2: ProviderURL default is the Cloudflare API
// base; previously it was the Hetzner DNS console.
func TestManualDNSProvider_ProviderURLOverride(t *testing.T) {
	cap := &captureStderr{}
	p, err := NewManualDNSProvider(DNSProviderConfig{
		Zone:        "example.com",
		ProviderURL: "https://console.aws.amazon.com/route53",
		Stderr:      cap,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.UpsertRecord(context.Background(), "faas-node-a.example.com", "10.0.0.1"); err == nil {
		t.Error("UpsertRecord must return sentinel error")
	}
	out := cap.String()
	// Round 2: default URL is Cloudflare; override replaces
	// it. Verify the override actually wins AND that no
	// Hetzner-shaped path leaked through (the previous bug).
	if !strings.Contains(out, "console.aws.amazon.com/route53") {
		t.Errorf("ProviderURL override not applied:\n%s", out)
	}
	if strings.Contains(out, "hetzner.com") {
		t.Errorf("stderr still mentions hetzner.com despite ProviderURL override:\n%s", out)
	}
}

// Concurrent writes must not interleave their curls.
func TestManualDNSProvider_ConcurrentWritesSerialized(t *testing.T) {
	cap := &captureStderr{}
	p, _ := NewManualDNSProvider(DNSProviderConfig{Zone: "example.com", Stderr: cap})
	const N = 50
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = p.UpsertRecord(context.Background(), "faas-node-a.example.com", "1.2.3.4")
		}()
	}
	wg.Wait()
	out := cap.String()
	count := strings.Count(out, "POST 'https://api.cloudflare.com/client/v4/zones/<ZONE_ID>/dns_records'")
	if count < N {
		t.Errorf("expected >= %d POST lines, got %d:\n%s", N, count, out)
	}
}

// NewDNSProvider dispatch table — pins the FAAS_DNS_PROVIDER
// env-var contract.
func TestNewDNSProvider_DispatchTable(t *testing.T) {
	cap := &captureStderr{}
	cfg := DNSProviderConfig{
		Zone:        "example.com",
		Stderr:      cap,
		SealedToken: []byte("dummy-token-not-unsealed-for-manual"),
	}

	// manual is reachable without an unsealed secret.
	p, err := NewDNSProvider(cfg, "manual")
	if err != nil {
		t.Fatalf("NewDNSProvider(manual): %v", err)
	}
	if _, ok := p.(*ManualDNSProvider); !ok {
		t.Errorf("manual dispatch returned %T, want *ManualDNSProvider", p)
	}

	// cloudflare requires an unsealable secret. The default
	// shim returns errSecretBoxUnconfigured; the test asserts
	// that error surfaces (operators see it at boot).
	// (Production wires the real secretbox shim at
	// cmd/gatewayd-public/main.go startup.)
	if _, err := NewDNSProvider(cfg, "cloudflare"); err == nil {
		t.Errorf("NewDNSProvider(cloudflare) must error without unseal shim")
	}

	// Unknown / unset must error explicitly.
	if _, err := NewDNSProvider(cfg, ""); err == nil {
		t.Errorf("NewDNSProvider('') must error")
	}
	if _, err := NewDNSProvider(cfg, "hetzner"); err == nil {
		t.Errorf("NewDNSProvider('hetzner') must error — provider was deleted in round 2")
	}
	if _, err := NewDNSProvider(cfg, "route53"); err == nil {
		t.Errorf("NewDNSProvider('route53') must error")
	}
}

// CloudflareRecordProvider construction path: empty Zone +
// empty token must error loudly before any HTTP request fires.
func TestCloudflareRecordProvider_ConstructionErrors(t *testing.T) {
	if _, err := NewCloudflareRecordProvider(DNSProviderConfig{}); err == nil {
		t.Errorf("empty cfg must error")
	}
	if _, err := NewCloudflareRecordProvider(DNSProviderConfig{Zone: "example.com"}); err == nil {
		t.Errorf("missing SealedToken must error")
	}
}
