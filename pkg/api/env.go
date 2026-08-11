package api

import (
	"fmt"
	"regexp"
)

// Customer env-var DTOs (issue #395 / ADR-045). The wire shape mirrors
// pkg/api/secrets.go EXCEPT there is no sealed-vs-plaintext boundary —
// env values are stored and re-emitted as-is by design (they're
// non-sensitive runtime config, distinct from the secret quota).
//
// Putting this file alongside secrets.go keeps the parallel surface
// obvious to reviewers and to the SDK generator (which walks
// pkg/api/*.go looking for response types).

// PutAppEnvRequest is the PUT /v1/apps/{slug}/env/{key} body. Value is
// stored verbatim in the app_envs table — no seal step, no ciphertext
// column. The byte cap is enforced against Limits.EnvValueMaxBytes by
// Validate() before the row reaches PG (defense in depth — see
// ValidateSecretKey's mirror comment).
type PutAppEnvRequest struct {
	// Value is the plaintext env-var value. Persisted as-is; treated as
	// non-sensitive by contract (issue #395 plaintext rationale +
	// ADR-045 §Decision). Customers must NOT use this surface for
	// credentials — sealed secrets remain the credential store.
	Value string `json:"value"`
}

// Validate enforces the byte cap against maxBytes. Used by apid's PUT
// handler so the cap is checked before the row ever hits the store.
// Returns *Problem directly (not error) so the call site can pass it
// straight to api.WriteProblem without an AsProblem unwrap.
func (r PutAppEnvRequest) Validate(maxBytes int) *Problem {
	if maxBytes > 0 && len(r.Value) > maxBytes {
		return ErrEnvVarValueTooLarge(Limits{EnvValueMaxBytes: maxBytes}, len(r.Value))
	}
	return nil
}

// AppEnvResponse is the GET / list shape. Mirrors AppSecretResponse
// exactly — the value NEVER appears here, only metadata about the env
// var. The customer gets the actual value at process start inside the
// guest (guest-init reads /etc/faas/env.json); the management API
// returns only the key set + timestamps so the dashboard can render
// "env: FOO, BAR" without echoing potentially sensitive defaults.
type AppEnvResponse struct {
	Key       string `json:"key"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// ScopedAppEnvResponse is the per-row shape for the nested
// `env_by_scope` response (ADR-090 D3). The flat AppEnvResponse
// shape is unchanged — the only new field is `scope`, which carries
// the scope name on the wire so a CLI / dashboard can render
// "scope: staging" without a second lookup. Value is NEVER echoed
// (same posture as AppEnvResponse).
//
// Reserved for the GET ?scope=__all__ path. The flat
// AppEnvListResponse with a single `env` array is the default
// (and is what every existing SDK caller decodes today). D3
// justifies the discriminated-union shape over a single flattened
// response because a customer on the default scope doesn't need
// scope metadata in every row — a one-off nested response is
// cheaper to render than a per-row scope string.
type ScopedAppEnvResponse struct {
	Scope     string `json:"scope"`
	Key       string `json:"key"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// EnvByScope is the nested map shape returned under `env_by_scope`
// when the GET carries `?scope=__all__`. Keys are scope names
// (e.g. "default", "staging"); values are the rows for that scope,
// ordered by key ASC to match the flat response. The map is nil
// (omitted via omitempty) for the default-scope GET so SDK callers
// that only care about the flat `env` array don't see a new field
// they have to special-case.
type EnvByScope map[string][]ScopedAppEnvResponse

// AppEnvListResponse is the wrapped GET response: the env slice plus
// quota metadata so the CLI can render "3/8 env vars" without a second
// request. Shape mirrors AppSecretListResponse field-for-field —
// SDK callers reuse the same parsing branch.
//
// EnvByScope is the discriminated-union arm for `?scope=__all__`
// (ADR-090 D3). When present, the SDK decodes env_by_scope and
// treats `env` as an empty array; when absent, the SDK decodes `env`
// as the flat per-scope result. Both arms are valid wire shapes
// for a GET /v1/apps/{slug}/envs; the `?scope=` query discriminates.
type AppEnvListResponse struct {
	Env        []AppEnvResponse     `json:"env"`
	EnvByScope EnvByScope           `json:"env_by_scope,omitempty"`
	Quota      int                  `json:"quota_max"`
	Count      int                  `json:"count"`
}

// ValidateEnvKey returns nil when key matches ^[A-Z][A-Z0-9_]*$ and is
// within MaxSecretKeyLen bytes; otherwise it returns the api.Problem-
// shaped CodeEnvVarInvalidKey. Returns *Problem directly (not error)
// so call sites can pass it straight to api.WriteProblem.
//
// This intentionally reuses SecretKeyPattern and MaxSecretKeyLen
// (rather than duplicating the regex literal) because POSIX env-var
// naming and the secrets naming surface share the same ASCII
// identifier grammar. Keeping one pattern avoids the drift where two
// regexes diverge over time. The DB CHECK in migration 00061 mirrors
// this regex verbatim.
func ValidateEnvKey(key string) *Problem {
	if key == "" {
		return ErrEnvVarInvalidKey("key is required")
	}
	if len(key) > MaxSecretKeyLen {
		return ErrEnvVarInvalidKey(fmt.Sprintf("key length %d exceeds max %d", len(key), MaxSecretKeyLen))
	}
	re := regexp.MustCompile(SecretKeyPattern)
	if !re.MatchString(key) {
		return ErrEnvVarInvalidKey("must start with a letter and contain only A-Z, 0-9, underscore")
	}
	return nil
}
