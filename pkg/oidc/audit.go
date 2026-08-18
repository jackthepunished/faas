// Audit-kind constants for the OIDC / keyless deploy path
// (issue #270 / ADR-101). Mirrors cmd/apid/audit.go:74
// (KindAuthLoginFailed) — typed constants live next to their only
// emit site so a future grep for "what audit kinds does pkg/oidc
// emit?" lands here.
//
// Two new kinds:
//
//   - auth.token.exchanged — every successful exchange. Data carries
//     {account_id, token_id, issuer_url, audience, subject_jti}. The
//     5-min TTL bounds how long this row is meaningful for incident
//     triage; the audit row IS the durable record (the exchanged
//     bearer is gone 5 min after mint).
//
//   - oidc.trust_policy.created — first-use auto-create of a trust
//     policy for a new (account, issuer) pair. Data carries
//     {account_id, issuer_url, jwks_url, audience, created_by:'auto'}.
//     The dashboard "refine" CTA uses this to distinguish "system
//     defaulted this" from "operator set this" (which is not yet
//     emitted; that path lands with the dashboard refine UI in PR-C).
package oidc

const (
	// KindAuthTokenExchanged is the per-exchange success audit kind.
	// Same dotted naming convention as auth.session.created (issue_session.go:74)
	// and auth.login (handlers_auth.go:157) — never an underscore
	// variant. A drift here is grep-able against the audit reader.
	KindAuthTokenExchanged = "auth.token.exchanged"

	// KindOIDCTrustPolicyCreated is the first-use auto-create audit kind.
	// Distinct from a future kind=oidc.trust_policy.updated which
	// PR-C will introduce for dashboard-driven refines.
	KindOIDCTrustPolicyCreated = "oidc.trust_policy.created"
)
