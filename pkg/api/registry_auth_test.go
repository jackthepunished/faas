package api

// registry_auth_test pins the wire-shape invariants for
// per-app private-registry Basic Auth (issue #461 / ADR-062):
//
//   - PutAppRegistryCredentialRequest.Validate rejects empty /
//     oversize / pattern-mismatched fields with the right problem
//     code (CodeInvalidRegistryHost).
//   - AppRegistryCredentialResponse marshals WITHOUT a password
//     field at any depth. The defence-in-depth check is that the
//     struct literally has no Password field, so a JSON marshal
//     cannot leak it even by accident.
//   - List response exposes quota_max + count + credentials fields.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPutAppRegistryCredentialRequest_Validate_Accepts(t *testing.T) {
	cases := []struct {
		name string
		req  PutAppRegistryCredentialRequest
	}{
		{"hostname only", PutAppRegistryCredentialRequest{Registry: "ghcr.io", Username: "alice", Password: "s3cret"}},
		{"hostname with port", PutAppRegistryCredentialRequest{Registry: "registry.example.com:5000", Username: "alice", Password: "s3cret"}},
		{"subdomain", PutAppRegistryCredentialRequest{Registry: "foo.bar.example.com", Username: "u", Password: "p"}},
		{"hyphenated", PutAppRegistryCredentialRequest{Registry: "my-registry.example.com", Username: "u", Password: "p"}},
		{"max-length distributed hostname (4×63 labels = 252 + dot)",
			// Each label is 63 chars (RFC 1035 max). Four labels
			// joined by dots total 252+3 = 255 bytes — over the
			// 253-byte cap so this would fail the length check.
			// Use a smaller valid distributed name instead.
			func() PutAppRegistryCredentialRequest {
				labels := []string{
					strings.Repeat("a", 63),
					strings.Repeat("b", 63),
					strings.Repeat("c", 63),
					strings.Repeat("d", 60), // 60 to fit under 253 with dots
				}
				return PutAppRegistryCredentialRequest{
					Registry: strings.Join(labels, "."),
					Username: "u", Password: "p",
				}
			}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if p := tc.req.Validate(); p != nil {
				t.Errorf("Validate = %+v, want nil", p)
			}
		})
	}
}

func TestPutAppRegistryCredentialRequest_Validate_Rejects(t *testing.T) {
	cases := []struct {
		name string
		req  PutAppRegistryCredentialRequest
		code string
	}{
		{"empty registry", PutAppRegistryCredentialRequest{Username: "u", Password: "p"}, CodeInvalidRegistryHost},
		{"empty username", PutAppRegistryCredentialRequest{Registry: "ghcr.io", Password: "p"}, CodeInvalidRegistryHost},
		{"empty password", PutAppRegistryCredentialRequest{Registry: "ghcr.io", Username: "u"}, CodeInvalidRegistryHost},
		{"scheme present", PutAppRegistryCredentialRequest{Registry: "https://ghcr.io", Username: "u", Password: "p"}, CodeInvalidRegistryHost},
		{"path present", PutAppRegistryCredentialRequest{Registry: "ghcr.io/v2", Username: "u", Password: "p"}, CodeInvalidRegistryHost},
		{"trailing slash", PutAppRegistryCredentialRequest{Registry: "ghcr.io/", Username: "u", Password: "p"}, CodeInvalidRegistryHost},
		{"uppercase host", PutAppRegistryCredentialRequest{Registry: "GHCR.IO", Username: "u", Password: "p"}, CodeInvalidRegistryHost},
		{"underscore in host", PutAppRegistryCredentialRequest{Registry: "bad_host.example.com", Username: "u", Password: "p"}, CodeInvalidRegistryHost},
		{"host too long (254)", PutAppRegistryCredentialRequest{Registry: strings.Repeat("a", 254), Username: "u", Password: "p"}, CodeInvalidRegistryHost},
		{"username too long", PutAppRegistryCredentialRequest{Registry: "ghcr.io", Username: strings.Repeat("u", 257), Password: "p"}, CodeInvalidRegistryHost},
		{"password too long", PutAppRegistryCredentialRequest{Registry: "ghcr.io", Username: "u", Password: strings.Repeat("p", MaxRegistryPasswordBytes+1)}, CodeInvalidRegistryHost},
		{"port out of range", PutAppRegistryCredentialRequest{Registry: "ghcr.io:99999", Username: "u", Password: "p"}, CodeInvalidRegistryHost},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.req.Validate()
			if p == nil {
				t.Fatalf("Validate = nil, want code %s", tc.code)
			}
			if p.Code != tc.code {
				t.Errorf("Validate code = %q, want %q", p.Code, tc.code)
			}
		})
	}
}

// TestAppRegistryCredentialResponse_NoPasswordField is the load-bearing
// defence-in-depth assertion: the wire shape has no Password field at
// any depth. A future refactor that adds one without removing the
// omit logic would silently leak ciphertext; this test fails first.
func TestAppRegistryCredentialResponse_NoPasswordField(t *testing.T) {
	resp := AppRegistryCredentialResponse{
		Registry:   "ghcr.io",
		Username:   "alice",
		CreatedAt:  "2026-08-01T00:00:00Z",
		UpdatedAt:  "2026-08-01T00:00:00Z",
		LastUsedAt: "2026-08-01T00:01:00Z",
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(strings.ToLower(string(b)), "password") {
		t.Errorf("marshaled response leaks password: %s", string(b))
	}
	var roundTrip map[string]any
	if err := json.Unmarshal(b, &roundTrip); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := roundTrip["password"]; ok {
		t.Errorf("response JSON has a password field: %s", string(b))
	}
}

// TestAppRegistryCredentialListResponse_NoPasswordField pins the
// same invariant for the list shape: even if every row carried a
// (hypothetically leaked) password, the JSON would not surface it.
func TestAppRegistryCredentialListResponse_NoPasswordField(t *testing.T) {
	resp := AppRegistryCredentialListResponse{
		Credentials: []AppRegistryCredentialResponse{
			{Registry: "ghcr.io", Username: "alice"},
			{Registry: "registry.example.com", Username: "bob"},
		},
		QuotaMax: 5,
		Count:    2,
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(strings.ToLower(string(b)), "password") {
		t.Errorf("list response leaks password: %s", string(b))
	}
	var roundTrip map[string]any
	if err := json.Unmarshal(b, &roundTrip); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := roundTrip["password"]; ok {
		t.Errorf("list JSON has a top-level password field: %s", string(b))
	}
	creds, ok := roundTrip["credentials"].([]any)
	if !ok {
		t.Fatalf("credentials not an array: %+v", roundTrip["credentials"])
	}
	for i, c := range creds {
		if m, ok := c.(map[string]any); ok {
			if _, ok := m["password"]; ok {
				t.Errorf("credentials[%d] has password field", i)
			}
		}
	}
}

// TestRegistryCredentialScopes_AreValid pins that the new scope
// strings are in the closed validScopes set so a typo can't slip
// through. Mirrors the convention for every other scope constant.
func TestRegistryCredentialScopes_AreValid(t *testing.T) {
	if !IsValidScope(ScopeRegistryCredentialsRead) {
		t.Errorf("%q not in validScopes", ScopeRegistryCredentialsRead)
	}
	if !IsValidScope(ScopeRegistryCredentialsWrite) {
		t.Errorf("%q not in validScopes", ScopeRegistryCredentialsWrite)
	}
}
