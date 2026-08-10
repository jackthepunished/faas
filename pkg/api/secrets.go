package api

import (
	"fmt"
	"regexp"
)

// Secret DTOs (spec §11/G2). Plaintext VALUES only appear in PutAppSecretRequest
// and never leave apid except transiently during the seal call
// (pkg/secretbox.Seal). All response shapes omit the value entirely.
//
// Naming mirrors the existing app/cron/domain resource shapes so the CLI
// (cmd/faas) and the dashboard can use the same JSON tags verbatim.

type PutAppSecretRequest struct {
	// Value is the plaintext. Sealed server-side with the host X25519
	// recipient and never persisted in plaintext. Maximum length is
	// enforced against Limits.SecretValueMaxBytes BEFORE the seal so
	// over-cap payloads never reach the seal path.
	Value string `json:"value"`
}

// Validate enforces the byte cap against maxBytes. Used by apid's PUT
// handler so the cap is checked before pkg/secretbox ever sees the value
// (defense in depth — secretbox.SealOne also checks).
//
// Returns *Problem directly (not error) so the call site can pass it
// straight to api.WriteProblem without an AsProblem unwrap.
func (r PutAppSecretRequest) Validate(maxBytes int) *Problem {
	if maxBytes > 0 && len(r.Value) > maxBytes {
		return ErrSecretValueTooLarge(Limits{SecretValueMaxBytes: maxBytes}, len(r.Value))
	}
	return nil
}

// AppSecretResponse is the GET / list shape. The value NEVER appears here —
// only metadata about the secret.
type AppSecretResponse struct {
	Key       string `json:"key"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	// Kid is the age-1... recipient string of the host identity
	// that sealed this row's ciphertext (ADR-089). Returns ""
	// for rows sealed before migration 00166 — those rows have
	// kid = NULL in PG; the JSON wire shape uses "" instead of
	// null for forward-compatibility with older SDKs that don't
	// handle null. Dashboards rendering "last rotated at" use
	// kid to filter on the host-key epoch.
	Kid string `json:"kid,omitempty"`
}

// AppSecretListResponse is the wrapped GET response: the secrets slice plus
// quota metadata so the CLI can render "3/25 secrets" without a second
// request. Matches the anonymous struct apid emits.
type AppSecretListResponse struct {
	Secrets []AppSecretResponse `json:"secrets"`
	Quota   int                 `json:"quota_max"`
	Count   int                 `json:"count"`
}

// AccountAppSecretResponse is one row in GET /v1/secrets — a sealed
// envelope on a specific app, returned alongside the owning app's
// identifier so the dashboard can render "foo-app / DATABASE_URL"
// without a parallel GET /v1/apps round-trip (issue #393).
//
// Ciphertext here is the age-sealed Envelope (base64). Plaintext
// NEVER appears in this struct, on the wire, or in logs — the same
// invariant that the per-app endpoint upholds (AppSecretResponse
// above). The handler is the only place that maps state → this DTO,
// and the mapping deliberately drops the per-row plaintext path:
// only the host X25519 recipient key unwraps, and that key never
// leaves the host.
type AccountAppSecretResponse struct {
	AppID      string `json:"app_id"`
	AppSlug    string `json:"app_slug"`
	Key        string `json:"key"`
	Ciphertext string `json:"ciphertext"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

// ListSecretsForAccountResponse is the page shape for GET /v1/secrets.
// Cursor is (app_slug, key) — pair-encoded as "<slug>|<key>" by the
// handler (the pgstore splits it back via split_part). Same JSON
// convention as InvoiceListResponse: empty NextBefore means the page
// is the end.
type ListSecretsForAccountResponse struct {
	Secrets    []AccountAppSecretResponse `json:"secrets"`
	NextBefore string                     `json:"next_before,omitempty"`
}

// ValidateSecretKey returns nil when key matches ^[A-Z][A-Z0-9_]*$ and is
// within MaxSecretKeyLen bytes; otherwise it returns the api.Problem-shaped
// CodeSecretInvalidKey. Returns *Problem directly (not error) so call sites
// can pass it straight to api.WriteProblem without an AsProblem unwrap — the
// key validation branch is hot enough that we want to skip the type assert.
//
// Mirror of the SQL CHECK constraint so upstream validation rejects bad keys
// before they reach the DB.
func ValidateSecretKey(key string) *Problem {
	if key == "" {
		return ErrSecretInvalidKey("key is required")
	}
	if len(key) > MaxSecretKeyLen {
		return ErrSecretInvalidKey(fmt.Sprintf("key length %d exceeds max %d", len(key), MaxSecretKeyLen))
	}
	re := regexp.MustCompile(SecretKeyPattern)
	if !re.MatchString(key) {
		return ErrSecretInvalidKey("must start with a letter and contain only A-Z, 0-9, underscore")
	}
	return nil
}

// RotateAppSecretRequest is the body of POST /v1/apps/{slug}/secrets/{key}
// /rotate (ADR-089). Same wire shape as PutAppSecretRequest — a single
// plaintext VALUE field. The byte cap is enforced here AND inside
// pkg/secretbox.SealOne (defense in depth — see PutAppSecretRequest).
//
// The "rotate" verb is intentionally distinct from PUT. Both endpoints
// write-or-replace the (app_id, key) row, but rotate is gated behind
// admin scope + MFA (matching secrets:write) and emits the secret.rotated
// audit kind when the row already had a value. PUT emits secret.set
// unconditionally. Dashboards filtering on kind='secret.rotated' see
// rotation events but not first-time sets.
type RotateAppSecretRequest struct {
	Value string `json:"value"`
}

// Validate enforces the byte cap against maxBytes. Same contract as
// PutAppSecretRequest.Validate so the rotate and PUT paths share the
// pre-seal cap enforcement.
func (r RotateAppSecretRequest) Validate(maxBytes int) *Problem {
	if maxBytes > 0 && len(r.Value) > maxBytes {
		return ErrSecretValueTooLarge(Limits{SecretValueMaxBytes: maxBytes}, len(r.Value))
	}
	return nil
}

// RotateAppSecretResponse is the success envelope for POST /v1/apps/{slug}
// /secrets/{key}/rotate. Returns the rotated key, RFC3339 timestamp, and
// the kid of the host identity that sealed the new envelope.
//
// kid lets dashboards render "rotated under <kid>" without a follow-up
// GET. Empty string means the row was rotated but the kid was not yet
// stampable (rare — happens only if apid started without host.age.pub,
// which the handler returns a 503 for instead).
type RotateAppSecretResponse struct {
	Key       string `json:"key"`
	RotatedAt string `json:"rotated_at"`
	Kid       string `json:"kid,omitempty"`
}
