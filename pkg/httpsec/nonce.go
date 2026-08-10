// Package httpsec emits the security hardening response headers
// required by spec §11 + DPA §TLS-on-the-dashboard. It is the only
// place in the codebase that sets Strict-Transport-Security,
// Content-Security-Policy, X-Frame-Options, X-Content-Type-Options,
// Referrer-Policy, and Permissions-Policy. gatewayd-internal and apid mount
// these middlewares at the outermost wrapper of their public
// listeners (cmd/gatewayd-internal/main.go publicHandler,
// cmd/apid/server.go observeWrap return).
//
// Static headers (Static) apply to every response. Content-Security-
// Policy with per-request nonces (Nonce) is gated: gatewayd-internal only
// emits it on requests that resolve to apid (so customer-app responses
// keep the customer's own CSP), apid emits it on every response (apid
// serves only dashboard + API; the JSON API carries a harmless CSP).
//
// The package does not log secrets. The CSP nonce is logged only as a
// 6-char fingerprint (zeroNonce[:6]) on the rare rand.Read failure
// path, so CodeQL go/log-injection sees no untrusted-data-to-log
// surface.
package httpsec

import (
	"context"
)

// nonceKey is the unexported context key for the per-request CSP nonce.
// Modeled on pkg/middleware.RequestIDKey (requestid.go:20).
type nonceKey struct{}

// WithNonce stores n on ctx and returns the new context. Empty n is a
// no-op so callers can pass through whatever the middleware handed
// them. Mirrors middleware.WithRequestID (requestid.go:24).
func WithNonce(ctx context.Context, n string) context.Context {
	if n == "" {
		return ctx
	}
	return context.WithValue(ctx, nonceKey{}, n)
}

// NonceFromContext extracts the CSP nonce for this request, returning
// "" if none was minted (e.g. unit tests, a request that bypassed the
// middleware). nil-safe via the typed empty-string default.
func NonceFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(nonceKey{}).(string); ok {
		return v
	}
	return ""
}
