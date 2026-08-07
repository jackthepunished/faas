// Tests for the manual DNS provider (Tier A8 / ADR-083). The
// Hetzner DNS provider's tests live in dns01_hetzner_test.go
// (the libdns surface) — the in-house HetznerRecordProvider is
// tested via the wire-shape contract below; full httptest
// coverage deferred to PR-C's drill target where the two-node
// Lima fleet exercises the full stack end-to-end.

package gateway

import (
	"bytes"
	"context"
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

func TestManualDNSProvider_UpsertRecordPrintsCurl(t *testing.T) {
	cap := &captureStderr{}
	p, err := NewManualDNSProvider(DNSProviderConfig{
		Zone:   "example.com",
		Stderr: cap,
	})
	if err != nil {
		t.Fatalf("NewManualDNSProvider: %v", err)
	}
	if err := p.UpsertRecord(context.Background(), "faas-node-a.example.com", "10.0.0.1"); err != nil {
		t.Fatalf("UpsertRecord: %v", err)
	}
	out := cap.String()
	// Must mention the record name + value so the operator can
	// confirm the curl targets the right record.
	if !strings.Contains(out, "faas-node-a.example.com") {
		t.Errorf("stderr missing record name:\n%s", out)
	}
	if !strings.Contains(out, "10.0.0.1") {
		t.Errorf("stderr missing record value:\n%s", out)
	}
	// Must be a POST to /records (the create path; idempotent
	// upsert → the manual path always POSTs, the operator
	// substitutes the zone id).
	if !strings.Contains(out, "POST") || !strings.Contains(out, "/records") {
		t.Errorf("stderr missing POST /records:\n%s", out)
	}
	if !strings.Contains(out, "example.com") {
		t.Errorf("stderr missing zone:\n%s", out)
	}
}

func TestManualDNSProvider_DeleteRecordPrintsCurl(t *testing.T) {
	cap := &captureStderr{}
	p, err := NewManualDNSProvider(DNSProviderConfig{
		Zone:   "example.com",
		Stderr: cap,
	})
	if err != nil {
		t.Fatalf("NewManualDNSProvider: %v", err)
	}
	if err := p.DeleteRecord(context.Background(), "faas-node-a.example.com"); err != nil {
		t.Fatalf("DeleteRecord: %v", err)
	}
	out := cap.String()
	if !strings.Contains(out, "DELETE") {
		t.Errorf("stderr missing DELETE verb:\n%s", out)
	}
	if !strings.Contains(out, "faas-node-a.example.com") {
		t.Errorf("stderr missing record name:\n%s", out)
	}
	if !strings.Contains(out, "example.com") {
		t.Errorf("stderr missing zone:\n%s", out)
	}
}

func TestManualDNSProvider_RejectsEmptyName(t *testing.T) {
	p, err := NewManualDNSProvider(DNSProviderConfig{Zone: "example.com"})
	if err != nil {
		t.Fatalf("NewManualDNSProvider: %v", err)
	}
	if err := p.UpsertRecord(context.Background(), "", "10.0.0.1"); err == nil {
		t.Errorf("UpsertRecord with empty name must error")
	}
	if err := p.DeleteRecord(context.Background(), ""); err == nil {
		t.Errorf("DeleteRecord with empty name must error")
	}
}

func TestManualDNSProvider_RejectsEmptyValue(t *testing.T) {
	p, err := NewManualDNSProvider(DNSProviderConfig{Zone: "example.com"})
	if err != nil {
		t.Fatalf("NewManualDNSProvider: %v", err)
	}
	if err := p.UpsertRecord(context.Background(), "faas-node-a.example.com", ""); err == nil {
		t.Errorf("UpsertRecord with empty value must error")
	}
}

func TestManualDNSProvider_RequiresZone(t *testing.T) {
	if _, err := NewManualDNSProvider(DNSProviderConfig{}); err == nil {
		t.Errorf("empty Zone must error at construction")
	}
}

// Concurrent writes must not interleave their curls. The lock
// inside ManualDNSProvider.write serialises them; without it
// the operator's `tee` would see two curls merged on one line.
func TestManualDNSProvider_ConcurrentWritesDoNotInterleave(t *testing.T) {
	cap := &captureStderr{}
	p, err := NewManualDNSProvider(DNSProviderConfig{
		Zone:   "example.com",
		Stderr: cap,
	})
	if err != nil {
		t.Fatalf("NewManualDNSProvider: %v", err)
	}
	const N = 10
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			name := "faas-node-a.example.com"
			_ = p.UpsertRecord(context.Background(), name, "10.0.0.1")
		}(i)
	}
	wg.Wait()
	// Each UpsertRecord emits one curl block. With N=10 writes
	// serialized by the provider's mutex, the captured output
	// should have N occurrences of the curl line. The exact
	// count is loose (the writes include header comments) —
	// the assertion is that the curl POST line appears at
	// least N times, no interleaving.
	out := cap.String()
	count := strings.Count(out, "POST 'https://dns.hetzner.com/api/v1/records'")
	if count < N {
		t.Errorf("expected >= %d POST lines, got %d:\n%s", N, count, out)
	}
}

// NewDNSProvider dispatch table — pins the FAAS_DNS_PROVIDER
// env-var contract.
func TestNewDNSProvider_DispatchTable(t *testing.T) {
	cap := &captureStderr{}
	cfg := DNSProviderConfig{
		Zone:       "example.com",
		Stderr:     cap,
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

	// hetzner requires an unsealable secret. The default
	// shim returns errSecretBoxUnconfigured; the test asserts
	// that error surfaces (operators see it at boot).
	if _, err := NewDNSProvider(cfg, "hetzner"); err == nil {
		t.Errorf("NewDNSProvider(hetzner) must error without unseal shim")
	}

	// Unknown / unset must error explicitly.
	if _, err := NewDNSProvider(cfg, ""); err == nil {
		t.Errorf("NewDNSProvider('') must error")
	}
	if _, err := NewDNSProvider(cfg, "route53"); err == nil {
		t.Errorf("NewDNSProvider('route53') must error")
	}
}

// HetznerRecordProvider construction path: empty Zone +
// empty token must error loudly before any HTTP request fires.
func TestHetznerRecordProvider_ConstructionErrors(t *testing.T) {
	if _, err := NewHetznerRecordProvider(DNSProviderConfig{}); err == nil {
		t.Errorf("empty cfg must error")
	}
	if _, err := NewHetznerRecordProvider(DNSProviderConfig{Zone: "example.com"}); err == nil {
		t.Errorf("missing SealedToken must error")
	}
}