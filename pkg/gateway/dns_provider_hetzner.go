// Hetzner DNS A/AAAA record provider for the Tier A8
// active-passive HA topology (ADR-083). Sibling to
// dns01_hetzner.go (which implements the libdns surface for
// CertMagic's ACME DNS-01 solver); this file implements the
// in-house DNSProvider surface for the leader-election DNS
// handoff. Same Hetzner DNS API endpoints, same Auth-API-Token
// header, but the record type is A/AAAA (vs TXT for DNS-01) and
// the name is `faas-<compute-node>.example.com` (vs
// `_acme-challenge.<host>` for DNS-01).
//
// Endpoints used:
//
//	GET    /api/v1/zones?name=<zone>           → list zones (find Zone ID by name)
//	POST   /api/v1/records                    → create a record (A, faas-<node>.<zone>)
//	PUT    /api/v1/records/<id>               → update an existing record (UpsertRecord idempotency)
//	DELETE /api/v1/records/<id>               → delete a record (DeleteRecord)
//
// Auth: header "Auth-API-Token: <token>" (the unsealed value
// from SealedToken via pkg/secretbox.OpenBytes).
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// hetznerDNSBaseURL is the Hetzner DNS API base. Override in
// tests via HetznerRecordProvider.APIURL.
const hetznerDNSBaseURL = "https://dns.hetzner.com/api/v1"

// HetznerRecordProvider implements DNSProvider against the
// Hetzner DNS API for the leader-election DNS handoff (Tier A8
// / ADR-083). Distinct from HetznerDNSProvider in
// dns01_hetzner.go, which implements the libdns surface for
// CertMagic's ACME DNS-01 solver (TXT records vs the A/AAAA
// records this file manages). Struct fields are unexported
// because callers should use NewHetznerRecordProvider.
type HetznerRecordProvider struct {
	token  string
	zone   string
	apiURL string
	hc     *http.Client

	// zoneIDOnce + zoneID cache the Zone → ZoneID resolution
	// (review finding #4: resolveZoneID previously fired
	// GET /zones on every UpsertRecord/DeleteRecord, doubling
	// Hetzner API call volume on every leader flip). The
	// resolution runs once per process; on a transient
	// 4xx/5xx (e.g. the zone was just created in another
	// process and the local cache missed), the next call
	// retries via zoneIDOnce's load-on-miss path: the
	// `defer` clears the cache on every error so the next
	// caller re-queries.
	zoneIDOnce sync.Once
	zoneID     string
}

// NewHetznerRecordProvider builds the Hetzner-backed
// DNSProvider. token is the unsealed Hetzner DNS API token
// (callers unseal from SealedToken via pkg/secretbox.OpenBytes
// before passing in). zone is the DNS zone name (e.g.
// "example.com"); the provider resolves Zone → ZoneID lazily
// on the first request and caches it for the lifetime of the
// process.
func NewHetznerRecordProvider(cfg DNSProviderConfig) (*HetznerRecordProvider, error) {
	if cfg.Zone == "" {
		return nil, fmt.Errorf("hetzner dns: Zone required")
	}
	if len(cfg.SealedToken) == 0 {
		return nil, fmt.Errorf("hetzner dns: SealedToken required")
	}
	// Unseal: the DNS_PROVIDER namespace is a SealedBytes blob
	// (distinct from the env-var Envelope used for webhook
	// secrets — ADR-076 precedent).
	plaintext, err := openDNSProviderToken(cfg.SealedToken)
	if err != nil {
		return nil, fmt.Errorf("hetzner dns: unseal token: %w", err)
	}
	apiURL := cfg.APIURL
	if apiURL == "" {
		apiURL = hetznerDNSBaseURL
	}
	return &HetznerRecordProvider{
		token:  string(plaintext),
		zone:   cfg.Zone,
		apiURL: apiURL,
		hc: &http.Client{
			Timeout: 10 * time.Second, // bounded — drain protocol is HADNSRecordStaleSeconds (30s)
		},
	}, nil
}

// openDNSProviderToken is the namespace-sealed unseal helper.
// It exists separately so tests can inject a fake OpenBytes
// without touching pkg/secretbox's surface. Today it
// round-trips through pkg/secretbox.OpenBytes with namespace
// "DNS_PROVIDER" — the same shape the webhook secretbox uses
// (ADR-076).
var openDNSProviderToken = func(sealed []byte) ([]byte, error) {
	return secretboxOpenDNSProvider(sealed)
}

// UpsertRecord implements DNSProvider. Idempotent: a record
// that already exists with the same value is a no-op (the
// provider returns nil without writing); a record with a
// different value is updated via PUT /records/<id>.
//
// name is the fully-qualified domain (e.g. "faas-node-a.
// example.com"); value is the leader's egress IP.
func (p *HetznerRecordProvider) UpsertRecord(ctx context.Context, name, value string) error {
	if name == "" || value == "" {
		return fmt.Errorf("hetzner dns: name and value required (got name=%q value=%q)", name, value)
	}
	zoneID, err := p.resolveZoneID(ctx)
	if err != nil {
		return err
	}
	existing, err := p.findRecord(ctx, zoneID, name)
	if err != nil {
		return err
	}
	if existing != nil && existing.Value == value {
		return nil // idempotent no-op
	}
	if existing != nil {
		return p.updateRecord(ctx, existing.ID, zoneID, name, value)
	}
	return p.createRecord(ctx, zoneID, name, value)
}

// DeleteRecord implements DNSProvider. Idempotent: a record
// that doesn't exist is a no-op (returns nil without an API
// call).
func (p *HetznerRecordProvider) DeleteRecord(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("hetzner dns: name required")
	}
	zoneID, err := p.resolveZoneID(ctx)
	if err != nil {
		return err
	}
	existing, err := p.findRecord(ctx, zoneID, name)
	if err != nil {
		return err
	}
	if existing == nil {
		return nil // idempotent no-op
	}
	return p.deleteRecord(ctx, existing.ID)
}

// resolveZoneID resolves Zone → ZoneID via
// GET /api/v1/zones?name=<zone>. Cached on the receiver
// (review finding #4) so a re-election cycle doesn't re-query
// the Hetzner API — the previous implementation fired
// GET /zones on every UpsertRecord/DeleteRecord, doubling the
// API call volume and risking the 1000 req/h/token free-tier
// quota. The cache is process-local; transient errors clear
// the cache so the next call retries.
func (p *HetznerRecordProvider) resolveZoneID(ctx context.Context) (string, error) {
	if p.zone == "" {
		return "", fmt.Errorf("hetzner dns: empty zone")
	}
	p.zoneIDOnce.Do(func() {
		id, err := p.queryZoneID(ctx)
		if err != nil {
			// Reset the Once so the next caller retries
			// (a transient 5xx shouldn't permanently
			// poison the cache; review finding #4).
			p.zoneIDOnce = sync.Once{}
			p.zoneID = ""
			return
		}
		p.zoneID = id
	})
	if p.zoneID == "" {
		// queryZoneID failed; retry on this call so the
		// operator sees the actual error (the Once
		// path above silently swallowed it to keep the
		// cache logic linear).
		return p.queryZoneID(ctx)
	}
	return p.zoneID, nil
}

// queryZoneID is the one-shot /zones?name=<zone> call that
// backs resolveZoneID's cache.
func (p *HetznerRecordProvider) queryZoneID(ctx context.Context) (string, error) {
	url := fmt.Sprintf("%s/zones?name=%s", p.apiURL, p.zone)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("hetzner dns: build zone request: %w", err)
	}
	req.Header.Set("Auth-API-Token", p.token)
	resp, err := p.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("hetzner dns: zone request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("hetzner dns: zone %s: status %d: %s", p.zone, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Zones []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"zones"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("hetzner dns: decode zone response: %w", err)
	}
	for _, z := range out.Zones {
		if z.Name == p.zone {
			return z.ID, nil
		}
	}
	return "", fmt.Errorf("hetzner dns: zone %q not found", p.zone)
}

// hetznerRecord mirrors the Hetzner /records response shape.
type hetznerRecord struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Name   string `json:"name"`
	Value  string `json:"value"`
	ZoneID string `json:"zone_id"`
}

// findRecord queries GET /records?zone_id=<zoneID>&name=<name>
// (review finding #5). The previous implementation fetched
// the unfiltered list and scanned client-side, which broke on
// a zone with >100 records: Hetzner paginates at 100/page and
// the response did not include the next-page URL, so a leader
// on page 2+ was invisible, UpsertRecord fell through to
// createRecord, and a duplicate A record was created against
// the same name — the round-robin DNS the `dns_stale` label
// was supposed to prevent. The `name=` filter is server-side,
// per Hetzner DNS API docs.
func (p *HetznerRecordProvider) findRecord(ctx context.Context, zoneID, name string) (*hetznerRecord, error) {
	url := fmt.Sprintf("%s/records?zone_id=%s&name=%s", p.apiURL, zoneID, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("hetzner dns: build records request: %w", err)
	}
	req.Header.Set("Auth-API-Token", p.token)
	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hetzner dns: records request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("hetzner dns: records list: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Records []hetznerRecord `json:"records"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("hetzner dns: decode records response: %w", err)
	}
	for i := range out.Records {
		// The `name=` filter is a substring match on
		// Hetzner's side; double-check client-side so a
		// same-suffix record (`faas-node-a.example.com`
		// vs `faas-node-a.example.com.attacker.tld`)
		// can't poison the lookup.
		if out.Records[i].Name == name && (out.Records[i].Type == "A" || out.Records[i].Type == "AAAA") {
			return &out.Records[i], nil
		}
	}
	return nil, nil
}

func (p *HetznerRecordProvider) createRecord(ctx context.Context, zoneID, name, value string) error {
	body, _ := json.Marshal(map[string]string{
		"zone_id": zoneID,
		"name":    name,
		"type":    "A",
		"value":   value,
		"ttl":     "60",
	})
	url := fmt.Sprintf("%s/records", p.apiURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("hetzner dns: build create request: %w", err)
	}
	req.Header.Set("Auth-API-Token", p.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.hc.Do(req)
	if err != nil {
		return fmt.Errorf("hetzner dns: create request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("hetzner dns: create record %s: status %d: %s", name, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

func (p *HetznerRecordProvider) updateRecord(ctx context.Context, recordID, zoneID, name, value string) error {
	body, _ := json.Marshal(map[string]string{
		"zone_id": zoneID,
		"name":    name,
		"type":    "A",
		"value":   value,
		"ttl":     "60",
	})
	url := fmt.Sprintf("%s/records/%s", p.apiURL, recordID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("hetzner dns: build update request: %w", err)
	}
	req.Header.Set("Auth-API-Token", p.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.hc.Do(req)
	if err != nil {
		return fmt.Errorf("hetzner dns: update request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("hetzner dns: update record %s: status %d: %s", recordID, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

func (p *HetznerRecordProvider) deleteRecord(ctx context.Context, recordID string) error {
	url := fmt.Sprintf("%s/records/%s", p.apiURL, recordID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("hetzner dns: build delete request: %w", err)
	}
	req.Header.Set("Auth-API-Token", p.token)
	resp, err := p.hc.Do(req)
	if err != nil {
		return fmt.Errorf("hetzner dns: delete request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("hetzner dns: delete record %s: status %d: %s", recordID, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
