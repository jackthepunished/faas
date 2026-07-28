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
// so the storage key is the SHA-256 of the raw token.
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

// API-key scopes (IAM-1, ADR-034 rev2). The first merge of the closed
// vocab (admin | read | write) was too coarse: granting `read` to a key
// gave the key every GET across the account surface (apps, usage,
// secrets audit, deployments), and `write` blocked the legitimate
// "deploy-only" CI key from reading the post-deploy logs it needed to
// gate a release. The fine-grained set below lets a customer mint a
// key that can only deploy (deploy:write), only read usage
// (usage:read), only read the app surface (apps:read), or only
// manage secrets (secrets:write) — without granting the other
// surfaces.
//
//	admin        — every action including billing, account deletion,
//	               key management.
//	apps:read    — GET /v1/apps, /v1/apps/{slug}, /v1/deployments,
//	               /v1/deployments/{id}, /v1/deployments/{id}/logs,
//	               /v1/apps/{slug}/instances, /v1/apps/{slug}/logs,
//	               /v1/apps/{slug}/secrets (list only), /v1/keys,
//	               /v1/audit-events, /v1/audit-events/{id},
//	               /v1/invocations, /v1/invocations/{id},
//	               /v1/delayed-tasks/{id}, /v1/account,
//	               /v1/account/export, /v1/crons, /v1/domains.
//	deploy:write — POST/PATCH/DELETE on /v1/apps, /v1/apps/{slug},
//	               /v1/domains, /v1/crons, /v1/invocations/queues/*,
//	               /v1/delayed-tasks, /v1/account/restore,
//	               /v1/apps/{slug}/invoke, /v1/apps/{slug}/invoke/async,
//	               /v1/apps/{slug}/deployments, /v1/apps/{slug}/wake,
//	               /v1/apps/{slug}/park, /v1/apps/{slug}/rollback,
//	               /v1/apps/{slug}/rename.
//	secrets:write— PUT/DELETE /v1/apps/{slug}/secrets/{key}.
//	usage:read   — GET /v1/usage, /v1/usage/summary.
//
// `admin` implicitly satisfies every other scope check — the
// principalHasScope helper grants any-of. Session-cookie auth (Key ==
// nil) is implicitly admin: humans at the dashboard always have full
// access.
//
// The closed vocabulary is mirrored at the DB layer by migration
// 00046's api_keys_scopes_vocab_chk CHECK constraint (and widened by
// migration 00063 to admit env:read/env:write for issue #395). The Go
// side is the first line; the constraint is the floor a typo cannot
// cross.
const (
	ScopeAdmin        = "admin"
	ScopeAppsRead     = "apps:read"
	ScopeDeployWrite  = "deploy:write"
	ScopeSecretsRead  = "secrets:read"
	ScopeSecretsWrite = "secrets:write"
	ScopeUsageRead    = "usage:read"
	// Issue #395 / ADR-045: env:read scopes the GET endpoint;
	// env:write scopes PUT/DELETE. Distinct codes from secrets:* so
	// the secret-quota bypass argument is closed — a customer can't
	// grant "secrets:write" through an env-var surface.
	ScopeEnvRead  = "env:read"
	ScopeEnvWrite = "env:write"
)

// validScopes is the closed set of scope strings the API accepts. The
// order is not significant — callers can pass scopes in any order.
var validScopes = map[string]struct{}{
	ScopeAdmin:        {},
	ScopeAppsRead:     {},
	ScopeDeployWrite:  {},
	ScopeSecretsRead:  {},
	ScopeSecretsWrite: {},
	ScopeUsageRead:    {},
	ScopeEnvRead:      {},
	ScopeEnvWrite:     {},
}

// IsValidScope reports whether s is in the allowed scope vocabulary.
func IsValidScope(s string) bool {
	_, ok := validScopes[s]
	return ok
}

// NormalizeCreateKeyScopes validates + defaults + dedupes the requested
// scopes for POST /v1/keys and the CLI exchange path.
//
//	empty   → [admin] (legacy default — preserve current behavior
//	          for SDK callers that have not yet learned about scopes).
//	unknown → error (wrap with %w so handlers can map the error to
//	          a 400 invalid_scope).
//	duplicates → collapsed; order preserved as-given.
//
// Single source of truth: every caller that mints an api_key row
// (handlers_ext.go::createKey, handlers_cli_auth.go::exchangeCliAuthCode)
// funnels through this helper so the DB CHECK constraint added in
// migration 00044 is the only remaining validation surface.
func NormalizeCreateKeyScopes(requested []string) ([]string, error) {
	if len(requested) == 0 {
		return []string{ScopeAdmin}, nil
	}
	seen := make(map[string]struct{}, len(requested))
	out := make([]string, 0, len(requested))
	for _, s := range requested {
		if !IsValidScope(s) {
			return nil, fmt.Errorf("unknown scope %q", s)
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out, nil
}

// Pre-baked per-route scope sets for the four common patterns in
// cmd/apid/server.go. Adding a new route should pick one of these
// named shapes; the literal scope-list form is reserved for routes
// that need an unusual combination (none today).
//
// Admin is always in every set because principalHasScope uses any-of
// semantics: an admin key always satisfies the route. A non-admin
// key must carry one of the other scopes in the set to be allowed.
var (
	// ScopesAdminOnly: route is destructive/privileged — only admin
	// keys (and session cookies, which are implicitly admin) pass.
	ScopesAdminOnly = []string{ScopeAdmin}

	// ScopesReadSurface: any read across the account's apps,
	// deployments, usage, audit, secrets-list, and config surface.
	// Granted by admin or apps:read.
	ScopesReadSurface = []string{ScopeAdmin, ScopeAppsRead}

	// ScopesUsageReadSurface: the two narrow usage endpoints.
	// Granted by admin or usage:read.
	ScopesUsageReadSurface = []string{ScopeAdmin, ScopeUsageRead}

	// ScopesSecretsWriteSurface: PUT/DELETE on
	// /v1/apps/{slug}/secrets/{key}. Granted by admin or
	// secrets:write.
	ScopesSecretsWriteSurface = []string{ScopeAdmin, ScopeSecretsWrite}

	// ScopesEnvWriteSurface: PUT/DELETE on /v1/apps/{slug}/env/{key}
	// (issue #395 / ADR-045). Granted by admin or env:write.
	// NOT MFA-gated because env vars are explicitly non-sensitive
	// runtime config (see handlers_env.go file header for the
	// trust-model rationale + ADR-045 §Decision).
	ScopesEnvWriteSurface = []string{ScopeAdmin, ScopeEnvWrite}

	// ScopesDeployWriteSurface: every deploy/mutate action except
	// secrets and key/admin operations. Granted by admin or
	// deploy:write.
	ScopesDeployWriteSurface = []string{ScopeAdmin, ScopeDeployWrite}
)
