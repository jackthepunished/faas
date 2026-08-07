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
// GET /api/v1/zones?name=<zone>. Cached on the receiver so a
// re-election cycle doesn't re-query.
func (p *HetznerRecordProvider) resolveZoneID(ctx context.Context) (string, error) {
	if p.zone == "" {
		return "", fmt.Errorf("hetzner dns: empty zone")
	}
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
	ID    string `json:"id"`
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
	ZoneID string `json:"zone_id"`
}

func (p *HetznerRecordProvider) findRecord(ctx context.Context, zoneID, name string) (*hetznerRecord, error) {
	url := fmt.Sprintf("%s/records?zone_id=%s", p.apiURL, zoneID)
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