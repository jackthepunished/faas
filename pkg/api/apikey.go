package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
)

// API keys (spec §4.2, §11). The plaintext key is shown to the user exactly once
// (at creation); only its SHA-256 is stored. Keys are prefixed so they are
// greppable in incident response and detectable in leaked-secret scanners.
const (
	// APIKeyPrefix marks live keys. A test/sandbox prefix can be added later.
	APIKeyPrefix = "fp_live_"
	// apiKeyRandomBytes is the entropy behind each key.
	apiKeyRandomBytes = 24
)

// GenerateAPIKey mints a new key, returning the plaintext (to show the user once)
// and its SHA-256 hash (to store). The plaintext is never persisted.
func GenerateAPIKey() (plaintext string, hash []byte, err error) {
	buf := make([]byte, apiKeyRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("api: generate key: %w", err)
	}
	plaintext = APIKeyPrefix + hex.EncodeToString(buf)
	sum := sha256.Sum256([]byte(plaintext))
	return plaintext, sum[:], nil
}

// HashAPIKey returns the SHA-256 of a plaintext key for lookup/comparison.
func HashAPIKey(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

// HashToken returns the SHA-256 of arbitrary raw bytes. Login tokens
// (M7.5 magic link) are random 32-byte values — no API-key prefix —
// so the storage key is the SHA-256 of the raw token, hex-decoded.
func HashToken(raw []byte) []byte {
	sum := sha256.Sum256(raw)
	return sum[:]
}

// ValidAPIKeyFormat reports whether s looks like one of our keys (cheap pre-check
// before hitting the database).
func ValidAPIKeyFormat(s string) bool {
	if !strings.HasPrefix(s, APIKeyPrefix) {
		return false
	}
	body := strings.TrimPrefix(s, APIKeyPrefix)
	if len(body) != apiKeyRandomBytes*2 {
		return false
	}
	_, err := hex.DecodeString(body)
	return err == nil
}

// ConstantTimeEqualHash compares two key hashes without leaking timing.
func ConstantTimeEqualHash(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

// API-key scopes (IAM-1, ADR-034). Every key carries an explicit set of
// scopes; the apid middleware checks the requested scope on each
// authenticated route. Unknown scopes are rejected at mint time so a
// typo cannot masquerade as a permissive scope. `admin` is the legacy
// "do everything" scope; `read` covers GETs; `write` covers POST/PUT/
// PATCH/DELETE. See ADR-034 for the rationale.
const (
	ScopeAdmin = "admin"
	ScopeRead  = "read"
	ScopeWrite = "write"
)

// validScopes is the closed set of scope strings the API accepts. The
// order is not significant — callers can pass scopes in any order.
var validScopes = map[string]struct{}{
	ScopeAdmin: {},
	ScopeRead:  {},
	ScopeWrite: {},
}

// IsValidScope reports whether s is in the allowed scope vocabulary.
func IsValidScope(s string) bool {
	_, ok := validScopes[s]
	return ok
}

// DefaultScopes is the scope set applied when a caller omits scopes on
// POST /v1/keys. Preserves the legacy "full access" behavior for SDK
// callers that have not yet learned about scopes. See ADR-034.
func DefaultScopes() []string {
	return []string{ScopeAdmin}
}

// MethodDefaultScope returns the scope that satisfies a request with the
// given HTTP method, assuming the route has no route-specific override.
// GETs are read-only; everything else is write. `admin` is implicitly
// allowed by every scope check, so this function names the non-admin
// scope required.
func MethodDefaultScope(method string) string {
	if method == "GET" {
		return ScopeRead
	}
	return ScopeWrite
}
