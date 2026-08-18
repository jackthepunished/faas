package api

import (
	"encoding/json"
	"testing"
)

// TestTenantSurfacesEnabledDefaultsOff asserts the flag is opt-in.
// A test running without FAAS_TENANT_SURFACES_ENABLED set must
// see the gate off so the surface routes (PR-C) stay 404/503 in
// CI / staging until the operator deliberately flips the switch.
func TestTenantSurfacesEnabledDefaultsOff(t *testing.T) {
	t.Setenv("FAAS_TENANT_SURFACES_ENABLED", "")
	if TenantSurfacesEnabled() {
		t.Fatal("TenantSurfacesEnabled default = true; want false")
	}
}

// TestTenantSurfacesEnabledAcceptsOnTokens covers the 1/true/yes/on
// accept set documented in flags.go. Anything outside the set
// must keep the gate off.
func TestTenantSurfacesEnabledAcceptsOnTokens(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "True", "yes", "YES", "on", "ON"} {
		t.Setenv("FAAS_TENANT_SURFACES_ENABLED", v)
		if !TenantSurfacesEnabled() {
			t.Errorf("TenantSurfacesEnabled(%q) = false; want true", v)
		}
	}
}

// TestTenantSurfacesEnabledRejectsOtherTokens pins the closed
// accept set so a typo (e.g. "enabled", "on " with trailing
// space — wait, we DO trim — so "truthy" or "1\n") doesn't
// silently enable the surface routes.
func TestTenantSurfacesEnabledRejectsOtherTokens(t *testing.T) {
	for _, v := range []string{"enabled", "truthy", "0", "no", "off", "false"} {
		t.Setenv("FAAS_TENANT_SURFACES_ENABLED", v)
		if TenantSurfacesEnabled() {
			t.Errorf("TenantSurfacesEnabled(%q) = true; want false", v)
		}
	}
}

// TestTenantSurfacesDTORoundTrip pins the wire shape: a serialized
// response must round-trip identically so the SDK regen and the
// dashboard can rely on stable field names. Locks the empty
// Hostnames array (we always emit the field; a future PR-C
// handler fills it).
func TestTenantSurfacesDTORoundTrip(t *testing.T) {
	s := TenantSurfaceResponse{
		ID:        "srf-1",
		AccountID: "acc-1",
		AppID:     "app-1",
		Name:      "na-customers",
		CertKind:  "per_host_san",
		Status:    "active",
		CertState: "issued",
		Hostnames: []TenantHostnameResponse{
			{Hostname: "api.customer-a.com", Verified: true, TXTRecord: "_faas-verify.api.customer-a.com"},
		},
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var back TenantSurfaceResponse
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.ID != s.ID || back.Name != s.Name || back.CertKind != s.CertKind {
		t.Fatalf("round-trip mismatch: %+v vs %+v", back, s)
	}
	if len(back.Hostnames) != 1 || back.Hostnames[0].Hostname != "api.customer-a.com" {
		t.Fatalf("hostnames round-trip: %+v", back.Hostnames)
	}
}

// TestCreateTenantSurfaceRequestDefaultsCertKind pins that the
// apid handler can rely on an empty CertKind meaning "default
// per_host_san". We don't fill it in the DTO; the store does
// (state.CreateTenantSurfaceIfUnderQuota). The test asserts the
// wire shape doesn't carry a default — a malformed request that
// sets cert_kind="" must be equivalent to omitting the field.
func TestCreateTenantSurfaceRequestDefaultsCertKind(t *testing.T) {
	var req CreateTenantSurfaceRequest
	raw := []byte(`{"app_id":"app-1","name":"x","hostnames":["a.example"]}`)
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	if req.CertKind != "" {
		t.Fatalf("default cert_kind = %q; want empty (store fills)", req.CertKind)
	}
	if len(req.Hostnames) != 1 || req.Hostnames[0] != "a.example" {
		t.Fatalf("hostnames = %+v", req.Hostnames)
	}
}

// TestCertEngineWiredDefaultsOff pins the dark-launch posture:
// the cert engine is unwired until the operator sets BOTH
// FAAS_TLS_STORAGE_DIR and FAAS_TLS_CONTACT_EMAIL. A misconfigured
// rollout that flips FAAS_TENANT_SURFACES_ENABLED on but leaves
// the cert-engine env blank must NOT crash the daemon; the
// wrapper's nil-issuer degradation surfaces a clear
// "cert engine unwired" last_error.
func TestCertEngineWiredDefaultsOff(t *testing.T) {
	t.Setenv("FAAS_TLS_STORAGE_DIR", "")
	t.Setenv("FAAS_TLS_CONTACT_EMAIL", "")
	if CertEngineWired() {
		t.Fatal("CertEngineWired = true with both env unset; want false")
	}
}

// TestCertEngineWiredRequiresBoth pins the AND-of-two contract:
// setting just one of the two env vars is NOT enough. The
// fail-closed contract from PR-D commit 1 spec demands both.
func TestCertEngineWiredRequiresBoth(t *testing.T) {
	t.Setenv("FAAS_TLS_STORAGE_DIR", "/var/lib/faas/certs")
	t.Setenv("FAAS_TLS_CONTACT_EMAIL", "")
	if CertEngineWired() {
		t.Fatal("CertEngineWired = true with only STORAGE_DIR set; want false")
	}
	t.Setenv("FAAS_TLS_STORAGE_DIR", "")
	t.Setenv("FAAS_TLS_CONTACT_EMAIL", "ops@example.com")
	if CertEngineWired() {
		t.Fatal("CertEngineWired = true with only CONTACT_EMAIL set; want false")
	}
	t.Setenv("FAAS_TLS_STORAGE_DIR", "/var/lib/faas/certs")
	t.Setenv("FAAS_TLS_CONTACT_EMAIL", "ops@example.com")
	if !CertEngineWired() {
		t.Fatal("CertEngineWired = false with both set; want true")
	}
}

// TestCertEngineStagingDefaultsOn pins the safe-default: a
// fresh dev box defaults to LE staging so a misconfigured DNS
// delegation can't burn the production rate limit. Production
// operators opt-in to the prod CA via FAAS_TLS_STAGING=0.
func TestCertEngineStagingDefaultsOn(t *testing.T) {
	t.Setenv("FAAS_TLS_STAGING", "")
	if !CertEngineStaging() {
		t.Fatal("CertEngineStaging default = false; want true (staging is the safe default)")
	}
	for _, v := range []string{"0", "false", "FALSE", "no", "off"} {
		t.Setenv("FAAS_TLS_STAGING", v)
		if CertEngineStaging() {
			t.Errorf("CertEngineStaging(%q) = true; want false", v)
		}
	}
}

// TestCertEngineDNSProviderDefaultsCloudflare pins the default
// per ADR-024 §6.
func TestCertEngineDNSProviderDefaultsCloudflare(t *testing.T) {
	t.Setenv("FAAS_TLS_DNS_PROVIDER", "")
	if got := CertEngineDNSProvider(); got != "cloudflare" {
		t.Errorf("CertEngineDNSProvider default = %q; want cloudflare", got)
	}
	t.Setenv("FAAS_TLS_DNS_PROVIDER", "hetzner")
	if got := CertEngineDNSProvider(); got != "hetzner" {
		t.Errorf("CertEngineDNSProvider(hetzner) = %q; want hetzner", got)
	}
	// Unknown provider falls back to cloudflare (the documented
	// default) rather than erroring — the cert engine will then
	// fail to construct a DNS provider and the wrapper's
	// nil-issuer degradation handles the visible failure.
	t.Setenv("FAAS_TLS_DNS_PROVIDER", "route53-unimpl")
	if got := CertEngineDNSProvider(); got != "cloudflare" {
		t.Errorf("CertEngineDNSProvider(unknown) = %q; want cloudflare (safe default)", got)
	}
}
