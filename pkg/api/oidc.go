// OIDC exchange DTOs (issue #270 / ADR-101). The struct tags
// match the openapi.yaml shape verbatim — the spec_compliance_test
// gate verifies both ends. The App field on the request is optional
// (omit-empty) so the bind-payload's audit-attribution path is
// conditional.
package api

// OIDCExchangeRequest is the body of POST /v1/auth/oidc/exchange.
// Provider is the customer-side IdP id (github/gitlab/circleci/oidc)
// used for audit attribution only — the issuer is pinned in the
// JWT `iss` claim and is what the server actually verifies against.
// Token is the raw IdP-issued JWT. Audience is the aud claim the
// customer pinned in the action and must match the trust policy's
// audience array verbatim. App is optional; it pins the exchange
// to one customer app so the audit row carries the app slug.
type OIDCExchangeRequest struct {
	Provider string `json:"provider"`
	Token    string `json:"token"`
	Audience string `json:"aud"`
	App      string `json:"app,omitempty"`
}

// OIDCExchangeResponse is the body of POST /v1/auth/oidc/exchange
// on success. Bearer is the opaque fp_oidc_<48 hex> token to put
// in Authorization: Bearer … on the deploy routes. ExpiresIn is
// the seconds-until-expiry (300 today, mirrored in OIDCBearerTTL).
// TokenID is the opaque row id useful for log correlation.
type OIDCExchangeResponse struct {
	Bearer    string `json:"bearer"`
	ExpiresIn int    `json:"expires_in"`
	TokenID   string `json:"token_id"`
}
