// Package httpsec emits the security hardening response headers
// required by spec §11 + DPA §TLS-on-the-dashboard. It is the only
// place in the codebase that sets Strict-Transport-Security,
// Content-Security-Policy, X-Frame-Options, X-Content-Type-Options,
// Referrer-Policy, and Permissions-Policy. gatewayd and apid mount
// these middlewares at the outermost wrapper of their public
// listeners (cmd/gatewayd/main.go publicHandler,
// cmd/apid/server.go observeWrap return).
//
// Static headers (Static) apply to every response. Content-Security-
// Policy with per-request nonces (Nonce) is gated: gatewayd only
// emits it on requests that resolve to apid (so customer-app responses
// keep the customer's own CSP), apid emits it on every response (apid
// serves only dashboard + API; the JSON API carries a harmless CSP).
//
// The package does not log secrets. The CSP nonce is logged only as a
// 6-char fingerprint (signOf) on the rare rand.Read failure path, so
// CodeQL go/log-injection sees no untrusted-data-to-log surface.
package httpsec

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"log/slog"
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

// nonceLen is the byte count fed into base64.RawURLEncoding; 16 bytes
// produces a 22-char URL-safe nonce, well above the 128-bit floor CSP
// recommends.
const nonceLen = 16

// NewNonce returns a fresh 22-character URL-safe nonce. crypto/rand so
// we don't pull a UUID dep just for this. On the extremely unlikely
// rand.Read failure we emit zero rather than panicking in the request
// hot path (modeled on middleware.NewRequestID at requestid.go:51-57);
// the worst case is one rendered page lacking strict CSP enforcement,
// and the caller logs the failure so the operator sees it.
func NewNonce() string {
	var b [nonceLen]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000000000000000000000"
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// newNonceOrLog is NewNonce with a non-nil logger so the rand.Read
// failure path emits a critical log line. nil logger is tolerated
// (slog.Default fallback) — the request hot path never blocks.
func newNonceOrLog(log *slog.Logger) string {
	if log == nil {
		log = slog.Default()
	}
	var b [nonceLen]byte
	if _, err := rand.Read(b[:]); err != nil {
		log.Error("httpsec: rand.Read failed; nonce degraded to zeros",
			"err", err)
		return "0000000000000000000000"
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// signOf returns a 6-char fingerprint of n suitable for structured
// logs without leaking the actual nonce. Used by the middleware on
// the rand.Read failure path so a debugger can correlate which
// request lost its CSP binding without recovering the full value.
// Empty n returns "".
func signOf(n string) string {
	if len(n) < 6 {
		return n
	}
	return n[:6]
}