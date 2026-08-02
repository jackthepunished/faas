package api

// Per-app private-registry Basic Auth DTOs (issue #461 / ADR-062).
// Mirror of pkg/api/registry_auth.go — kept in sync because the public
// SDK at sdk/go/internal/api/ is the surface the CLI and the dashboard
// bind to; pkg/api/ is the canonical wire definition that apid emits.
//
// Plaintext PASSWORD only appears in PutAppRegistryCredentialRequest and
// never leaves apid except transiently during secretbox.Seal. All
// response shapes omit the password entirely.
//
// The Registry field is the normalized host (lowercase, no scheme, no
// path, port preserved) — apid's normalizeRegistryHost is the single
// source of truth on the boundary.

import (
	"fmt"
	"regexp"
)

// MaxRegistryHostLen mirrors the SQL CHECK constraint
// (length(registry) <= 253, RFC 1035 hostname cap).
const MaxRegistryHostLen = 253

// MaxRegistryUsernameLen mirrors the SQL CHECK constraint
// (length(username) <= 256). Username is metadata, not sealed.
const MaxRegistryUsernameLen = 256

// MaxRegistryPasswordBytes is the per-request plaintext cap. 4 KiB
// mirrors SecretValueMaxBytes for the cheapest plan so a misconfigured
// PUT fails closed at the gate.
const MaxRegistryPasswordBytes = 4 * 1024

// registryHostRe is the normalized-host shape accepted by the SDK
// (applies AFTER normalizeRegistryHost drops the scheme). Anchored
// DNS name with optional :port. See pkg/api/registry_auth.go for the
// exact form. IPv6 literals are intentionally NOT accepted.
var registryHostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*(:([1-9][0-9]{0,3}|[1-5][0-9]{4}|6[0-4][0-9]{3}|65[0-4][0-9]{2}|655[0-2][0-9]|6553[0-5]))?$`)

// PutAppRegistryCredentialRequest is the PUT body. Registry MUST be
// already normalized by the caller (lowercase, no scheme, no path,
// no trailing slash); apid's normalizeRegistryHost is the single
// source of truth.
type PutAppRegistryCredentialRequest struct {
	Registry string `json:"registry"`
	Username string `json:"username"`
	// Password is plaintext at the wire boundary only. apid seals it
	// via secretbox.SealBytes before persisting; the plaintext is
	// never logged, audited, or echoed in error responses. Validate
	// enforces MaxRegistryPasswordBytes.
	Password string `json:"password"`
}

// Validate enforces the per-field byte caps. Returns *Problem directly
// so the call site can pass it straight to api.WriteProblem.
func (r PutAppRegistryCredentialRequest) Validate() *Problem {
	if r.Registry == "" {
		return ErrInvalidRegistryHost(fmt.Errorf("registry is required"))
	}
	if len(r.Registry) > MaxRegistryHostLen {
		return ErrInvalidRegistryHost(fmt.Errorf("registry length %d exceeds %d", len(r.Registry), MaxRegistryHostLen))
	}
	if !registryHostRe.MatchString(r.Registry) {
		return ErrInvalidRegistryHost(fmt.Errorf("registry %q does not match normalized host pattern (lowercase DNS[:port])", r.Registry))
	}
	if r.Username == "" {
		return ErrInvalidRegistryHost(fmt.Errorf("username is required"))
	}
	if len(r.Username) > MaxRegistryUsernameLen {
		return ErrInvalidRegistryHost(fmt.Errorf("username length %d exceeds %d", len(r.Username), MaxRegistryUsernameLen))
	}
	if r.Password == "" {
		return ErrInvalidRegistryHost(fmt.Errorf("password is required"))
	}
	if len(r.Password) > MaxRegistryPasswordBytes {
		return ErrInvalidRegistryHost(fmt.Errorf("password length %d exceeds %d", len(r.Password), MaxRegistryPasswordBytes))
	}
	return nil
}

// AppRegistryCredentialResponse is the GET / PUT response shape. The
// password NEVER appears here.
type AppRegistryCredentialResponse struct {
	Registry   string `json:"registry"`
	Username   string `json:"username"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
	LastUsedAt string `json:"last_used_at,omitempty"`
}

// AppRegistryCredentialListResponse is the wrapped GET response:
// rows + quota metadata.
type AppRegistryCredentialListResponse struct {
	Credentials []AppRegistryCredentialResponse `json:"credentials"`
	QuotaMax    int                             `json:"quota_max"`
	Count       int                             `json:"count"`
}
