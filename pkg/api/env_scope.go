package api

import (
	"regexp"
)

// EnvScopePattern is the regex enforced by the app_envs.scope CHECK
// constraint added in migration 00203 (ADR-090 PR-A) and mirrored
// verbatim by ValidateScope below. Mirrors the server-side validSlug
// regex at cmd/apid/handlers.go:600 — lowercase alnum + dash, 3..40
// chars, no leading/trailing dash. A scope is NOT a free-form string:
// it's a domain-valid slug that the operator can reference from
// `gregale env set --scope staging KEY=val` (PR-B) and from the
// `?scope=` query param on /v1/apps/{slug}/envs (PR-B).
//
// The shape is intentionally the same as apps.slug so scope-based env
// overrides can be addressed by the same identifier the operator
// already uses for app slugs. Keeping the regex here as a named const
// (rather than a regexp.MustCompile at the call site) means a future
// relaxation — e.g. widening to underscores — only touches one file
// and the DB CHECK migration. See EnvScopeAllSentinel for the one
// string the regex must continue to reject on the write path.
const EnvScopePattern = `^[a-z0-9]([a-z0-9-]{1,38})[a-z0-9]$`

// MaxEnvScopeLen bounds the scope name. Mirrors MaxSecretKeyLen /
// MaxOrgSlugLen so the wire limits and DB CHECK share one source.
// 40 chars is the upper bound the migration's CHECK allows
// (`^[a-z0-9]([a-z0-9-]{1,38})[a-z0-9]$` = exactly 3..40 chars).
const MaxEnvScopeLen = 40

// EnvScopeAllSentinel is the magic string a client passes in
// `?scope=__all__` to request a nested `env_by_scope` response shape
// (ADR-090 D3). It MUST continue to fail EnvScopePattern so the
// sentinel cannot collide with a real scope name. Rejecting it via
// pattern alone would be brittle (a future relaxation that admits
// underscores would accidentally start accepting "__all__"); instead
// ValidateScope does a two-stage check: regex first, then a
// dedicated sentinel rejection on the write path. See
// ErrEnvScopeReserved for the 400 error code.
const EnvScopeAllSentinel = "__all__"

// envScopeRe is the compiled form of EnvScopePattern. Compiled once
// at init so each ValidateScope call is a MatchString and not a
// MustCompile. The pattern is small + constant — no need for sync.Once.
var envScopeRe = regexp.MustCompile(EnvScopePattern)

// ValidateScope returns nil when s is a well-formed scope name; otherwise
// it returns one of:
//
//   - ErrEnvScopeReserved (400) when s == EnvScopeAllSentinel
//     (`__all__`) — the sentinel is reserved for the read-path nested
//     `env_by_scope` response and MUST NOT be set as a scope on
//     write. Distinct from ErrEnvScopeInvalid so a CLI author can
//     tell "you accidentally used the all-scopes sentinel" apart from
//     "your scope name has the wrong shape".
//   - ErrEnvScopeInvalid (400) for any other rejection: empty,
//     exceeds MaxEnvScopeLen, or fails EnvScopePattern.
//
// Returns *Problem directly (not error) so call sites can pass it
// straight to api.WriteProblem without an AsProblem unwrap. This
// matches the contract of ValidateEnvKey / ValidateSecretKey on the
// sibling surfaces in pkg/api/{env,secrets}.go.
//
// Callers: apid's PUT/DELETE /v1/apps/{slug}/envs/{key}?scope=...
// (rejects invalid scope names before they reach the store) and the
// gregale CLI's `env set --scope=...` (same 400 problem code so the
// dashboard renders one consistent error card).
func ValidateScope(s string) *Problem {
	if s == EnvScopeAllSentinel {
		return ErrEnvScopeReserved(EnvScopeAllSentinel)
	}
	if s == "" {
		return ErrEnvScopeInvalid("scope is required")
	}
	if len(s) > MaxEnvScopeLen {
		return ErrEnvScopeInvalid(
			"scope length exceeds max",
		)
	}
	if !envScopeRe.MatchString(s) {
		return ErrEnvScopeInvalid(
			"scope must match " + EnvScopePattern,
		)
	}
	return nil
}
