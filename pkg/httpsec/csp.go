// csp.go — Content-Security-Policy with per-request nonce, gated by
// a path predicate so gatewayd doesn't lock CSP onto customer-app
// proxied responses (those apps govern their own CSP). apid emits
// CSP on every response (apid serves only dashboard + API; JSON
// responses carry a harmless policy).
//
// The header literal is pinned by issue #249. Editing it requires
// touching every dashboard template that holds a matching
// `nonce="…"` attribute — see pkg/dashboard/templates/*.html.

package httpsec

import (
	"log/slog"
	"net/http"
	"strings"
)

// BuildCSPForTest exposes the unexported buildCSP to the test
// package without exposing it on the public API. Test-only seam:
// live code never calls it (the Nonce middleware inlines the build).
//
// The exported name carries the _ForTest suffix so a code-review
// reader knows immediately that this is a test-only handle. The
// _test.go file imports the package via the external test package
// (package httpsec_test) and reads it as httpsec.BuildCSPForTest.
var BuildCSPForTest = buildCSP

// cspScriptHosts is the additional script-src host set beyond 'self'
// and the per-request nonce. Issue #249 keeps the dashboard's
// htmx.org@2.0.4 and htmx-ext-sse@2.2.2 scripts served from
// unpkg.com — the alternative (self-hosting) is tracked as a future
// ADR. SRI hashes would tighten this further.
var cspScriptHosts = []string{"https://unpkg.com"}

// cspFormActionHosts is the form-action host list. Today no client-
// side Stripe.js is loaded (verified by grep), but the policy
// anticipates Stripe Checkout / Billing Portal onboarding.
var cspFormActionHosts = []string{
	"https://*.stripe.com",
	"https://billing.faas.example",
}

// buildCSP returns the Content-Security-Policy header value for the
// given nonce. The output is byte-stable for a given nonce so the
// test suite can assert exact equality (TestCSP_HeaderMatchesDashboardSpec).
//
// `'nonce-…'` MUST be quoted exactly: unquoted nonces match any
// per-page nonce value, which defeats the purpose. CSP3 source-list
// grammar, https://www.w3.org/TR/CSP3/#match-url-to-source-list.
//
// Host sources (https://unpkg.com, https://*.stripe.com) are
// unquoted — only keywords like 'self' / 'nonce-…' / 'unsafe-inline'
// take quotes. The `default-src 'self'` covers anything not
// enumerated below (frame-src, font-src, media-src, object-src,
// etc.); the few directives we override are the ones the dashboard
// actually uses.
//
// img-src allows data: so the dashboard can inline small icons /
// SVG without an XSS-shaped detour; a future PR can tighten when
// payment icons land.
func buildCSP(nonce string) string {
	var b strings.Builder
	b.WriteString("default-src 'self'; ")
	b.WriteString("script-src 'self' 'nonce-")
	b.WriteString(nonce)
	b.WriteString("'")
	for _, h := range cspScriptHosts {
		b.WriteString(" ")
		b.WriteString(h)
	}
	b.WriteString("; ")
	b.WriteString("style-src 'self' 'nonce-")
	b.WriteString(nonce)
	b.WriteString("'; ")
	b.WriteString("img-src 'self' data:; ")
	b.WriteString("connect-src 'self'; ")
	b.WriteString("frame-ancestors 'none'; ")
	b.WriteString("base-uri 'none'; ")
	b.WriteString("form-action 'self'")
	for _, h := range cspFormActionHosts {
		b.WriteString(" ")
		b.WriteString(h)
	}
	b.WriteString("'")
	return b.String()
}

// Nonce is the CSP-emitting middleware. It mints a fresh nonce per
// request, stores it on the request context (so pkg/dashboard.Render
// can stamp `nonce="…"` on every <script>/<style> tag), and emits
// Content-Security-Policy only when gate(r) returns true.
//
// gate is the apid-vs-customer-app discriminator for gatewayd
// (cmd/gatewayd/proxy.go::isApidPath); apid passes a func that
// always returns true. Forgetting the gate is a compile error in
// gatewayd's main.go (the parameter is required).
//
// The middleware does not log the nonce; on the rare rand.Read
// failure path it logs the 6-char fingerprint so the operator can
// correlate the request without recovering the value.
func Nonce(gate func(*http.Request) bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nonce := newNonceOrLog(slog.Default())
		if nonce == "0000000000000000000000" {
			// rand.Read failed; surface to the structured log so
			// the operator sees it. signOf (first 6 chars) keeps
			// the full nonce out of any log line.
			slog.Default().Warn("httpsec: nonce degraded",
				"path", r.URL.Path, "nonce_fp", signOf(nonce))
		}
		r = r.WithContext(WithNonce(r.Context(), nonce))
		if gate != nil && gate(r) {
			w.Header().Set("Content-Security-Policy", buildCSP(nonce))
		}
		next.ServeHTTP(w, r)
	})
}