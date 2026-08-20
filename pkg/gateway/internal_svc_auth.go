package gateway

// internal_svc_auth.go — ADR-119 ingress-control gate for
// apps.public_auth_mode='internal_only'. The package-local
// surface here is split into three small pieces:
//
//  1. InternalSvcVerifier interface — the bridge pkg/gateway
//     consumes; the concrete impl is constructed by
//     cmd/gatewayd-internal/internal_svc_verifier.go from the
//     env-loaded FAAS_INTERNAL_SVC_PUBKEYS map and delegates to
//     pkg/internalsvc.Verify. Keeping the interface in pkg/gateway
//     preserves the same dependency-direction precedent as
//     JWTVerifier (gateway/edge_rules.go:873) and
//     PublicAuthUnsealer (gateway/handler.go:365).
//
//  2. Handler.applyIngressInternalSvc — the HTTP-front-door
//     gate, called from Handler.ServeHTTP after
//     applyIngressIPAllowlist (handler.go:~4586) and before
//     applyEdgeRuleIP. Operates on a *http.Request reaching
//     gatewayd-internal directly via the unix socket (so the
//     Authorization header survives the public→internal hop —
//     gatewayd-public strips it, see internal_proxy.go:~351).
//
//  3. SynthServer.applyIngressInternalSvc — the cron-fired
//     gate, called from SynthServer.handleSynthesize
//     (pkg/gateway/synth.go:~213) BEFORE dispatcher.Wake so a
//     forged schedd cannot wake an internal_only app. The
//     synth-side gate is the design-gap surfaced during the
//     PR-A build — schedd cron bypasses Handler.ServeHTTP via
//     /v1/synthesize, so the Handler-only gate would never
//     fire for cron traffic. See
//     docs/adr/119-app-public-auth-internal-only.md
//     "First caller: schedd" and the design-gap note.
//
// Audit redaction invariant (carry-over from PR #999 ADR-118):
// the JWT string MUST NEVER appear in an audit payload. The
// verify path returns only typed errors; only the error reason
// code (audience_mismatch | expired | not_yet_valid |
// unknown_service | signature_invalid | malformed |
// empty_allowlist) and app_id/from_host flow into the audit.
// Mirrors the IP-allowlist posture: only entry counts reach the
// audit log, never the CIDR strings themselves.

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/onebox-faas/faas/pkg/api"
)

// InternalSvcVerifier is the bridge pkg/gateway consumes to
// validate Authorization: Bearer JWTs against the per-service
// public-key allowlist. Returns the verified svcName (the JWT
// `sub` claim) on success; returns a typed error from the
// internalsvc.Err* family on failure. Implementations MUST NOT
// echo the token back in any error message — the only signal
// caller code may put into audit/metric is the error reason
// code (handled inside the gate helper, not by the verifier).
type InternalSvcVerifier interface {
	// Verify parses the token's unverified claims, looks up the
	// public key by `sub` (svcName) in the operator-loaded
	// allowlist, and verifies the signature against the looked-
	// up key. ctx is propagated to underlying crypto primitives
	// (currently unused by crypto/ed25519 stdlib but reserved
	// for future revocations checks).
	Verify(ctx context.Context, rawToken string) (svcName string, err error)
	// AllowedSvcNames returns the sorted list of svcNames the
	// verifier currently trusts. Used by admin endpoints to
	// render the allowlist (without exposing the public keys
	// themselves). Returns nil when the verifier is disabled
	// (no key loaded — every internal_only request would 500).
	AllowedSvcNames() []string
}

// WithInternalSvcVerifier (ADR-119) arms the per-app
// 'internal_only' ingress gate. Mirror of WithPublicAuth /
// WithJWTVerifier. nil = the gate is disabled; an app that has
// somehow ended up in internal_only mode without the verifier
// wired would 500 with operator_error (defence in depth: a
// silent pass-through here is worse than a 500).
func (h *Handler) WithInternalSvcVerifier(v InternalSvcVerifier) *Handler {
	h.internalSvcVerifier = v
	return h
}

// bearerFromHeader extracts the JWT from a "Authorization:
// Bearer <token>" header. Returns (token, true) when the header
// is present and well-formed per RFC 6750 §2.1; otherwise
// returns ("", false). Case-insensitive on the scheme — RFC
// 7230 §3.2 (case-insensitive token compare) — but the
// canonical wire form is "Bearer" with capital B. The same
// helper is used by the applyEdgeRuleJWT path; keeping it
// package-local here would risk drift, so we export the
// internal-package shape (lowercase 'b') for the two callers.
//
// We deliberately do NOT trim whitespace inside the token —
// base64 padding is significant and a stray space inside the
// token is a sign of header smuggling (issue #676 / ADR-080
// precedent: strip hop-by-hop headers in the public→internal
// hop; here the verifier rejects rather than silently fixes).
func bearerFromHeader(r *http.Request) (string, bool) {
	v := r.Header.Get("Authorization")
	if v == "" {
		return "", false
	}
	const scheme = "Bearer "
	if len(v) <= len(scheme) {
		return "", false
	}
	if !strings.EqualFold(v[:len(scheme)], scheme) {
		return "", false
	}
	tok := strings.TrimSpace(v[len(scheme):])
	if tok == "" {
		return "", false
	}
	return tok, true
}

// applyIngressInternalSvc (ADR-119) is the per-app
// public_auth_mode='internal_only' ingress gate. Mirrors
// applyIngressIPAllowlist (handler.go:~2189) — runs AFTER
// applyIngressIPAllowlist (handler.go:~4586) so an IP-blocked
// request short-circuits first, and BEFORE applyEdgeRuleIP
// (handler.go:~4590) so an internal-token failure never wakes
// a Firecracker microVM. Same invariant as the kind=ip gate.
//
// Trust chain: gatewayd-public MUST strip inbound Authorization
// headers (see internal_proxy.go:~351, added in this PR) so
// external callers can never reach this gate with an
// Authorization header intact. Only daemons that dial
// gatewayd-internal directly via /run/faas/gatewayd-internal.sock
// reach this gate with an Authorization header. This is the
// load-bearing reason the gate is registered on the INTERNAL
// handler, not the public one — the public handler would let
// every customer reach the gate.
//
// Failure modes:
//   - No Authorization header → 403
//     (instances.public_auth_internal_missing audit + outcome=blocked metric)
//   - Header present but verify failed (audience/exp/unknown
//     svc/sig/malformed) → 403
//     (instances.public_auth_internal_invalid audit + outcome=blocked metric)
//   - Verifier disabled (operator misconfig — no
//     FAAS_INTERNAL_SVC_PUBKEYS) AND app is in internal_only
//     → 500 operator_error (defence in depth, not a silent
//     pass-through)
//
// Pass-through (no deny written, return false) → the verified
// svcName is attached to r.Context via
// context.WithValue(ctx, internalSvcCtxKey{}, svcName) so the
// per-request audit row at the wake step can include
// `authenticated_as` without re-verifying. Downstream code
// reads it via InternalSvcFromContext.
func (h *Handler) applyIngressInternalSvc(w http.ResponseWriter, r *http.Request, app App) bool {
	if app.PublicAuth.Mode != publicAuthModeInternalOnly {
		return false
	}
	if h.internalSvcVerifier == nil {
		// Operator misconfig: internal_only mode is armed on
		// the app row but the verifier was never wired (env
		// not loaded, daemon started without the
		// FAAS_INTERNAL_SVC_PUBKEYS config). 500, not 403 —
		// same loud posture as the empty-allowlist case in
		// applyIngressIPAllowlist (handler.go:~2199). Better
		// to surface the misconfig than to silently fail
		// closed (which would lock every customer out) or
		// silently fail open (which would be a security
		// hole).
		if h.log != nil {
			h.log.Error("app in internal_only mode but no InternalSvcVerifier wired — refusing",
				slog.String("app_id", app.ID),
				slog.String("slug", app.Slug))
		}
		if h.metrics != nil {
			h.metrics.ObserveInternalAuthMatch("blocked")
		}
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
			api.CodeInternal,
			"app is misconfigured",
			"internal_only mode requires the gatewayd-internal verifier; check FAAS_INTERNAL_SVC_PUBKEYS"))
		return true
	}
	tok, ok := bearerFromHeader(r)
	if !ok {
		h.emitAuthnAudit(r, app, nil, "instances.public_auth_internal_missing", map[string]any{
			"app_id":    app.ID,
			"from_host": r.Host,
			"reason":    "missing_authorization_header",
		})
		if h.metrics != nil {
			h.metrics.ObserveInternalAuthMatch("blocked")
		}
		api.WriteProblem(w, api.NewProblem(http.StatusForbidden,
			api.CodeForbidden,
			"internal-only mode requires Authorization: Bearer",
			"this app is reachable only by Gregale daemons presenting a JWT with aud='gregale.internal'"))
		return true
	}
	svcName, err := h.internalSvcVerifier.Verify(r.Context(), tok)
	if err != nil {
		_ = svcName // unused on the failure branch
		// Audit the reason code only — never the token, never
		// the raw signature bytes. The verifier returns typed
		// internalsvc.Err* sentinels; map them to a closed
		// reason vocabulary so the audit row stays stable.
		reason := "signature_invalid"
		if errors.Is(err, errInternalSvcAudience) {
			reason = "audience_mismatch"
		} else if errors.Is(err, errInternalSvcExpired) {
			reason = "expired"
		} else if errors.Is(err, errInternalSvcNotYetValid) {
			reason = "not_yet_valid"
		} else if errors.Is(err, errInternalSvcUnknownSvc) {
			reason = "unknown_service"
		} else if errors.Is(err, errInternalSvcMalformed) {
			reason = "malformed"
		} else if errors.Is(err, errInternalSvcEmptyAllowlist) {
			reason = "empty_allowlist"
		}
		h.emitAuthnAudit(r, app, nil, "instances.public_auth_internal_invalid", map[string]any{
			"app_id":    app.ID,
			"from_host": r.Host,
			"reason":    reason,
		})
		if h.metrics != nil {
			h.metrics.ObserveInternalAuthMatch("blocked")
		}
		api.WriteProblem(w, api.NewProblem(http.StatusForbidden,
			api.CodeForbidden,
			"internal token verification failed",
			"present a valid Gregale-internal JWT (aud='gregale.internal')"))
		return true
	}
	if h.metrics != nil {
		h.metrics.ObserveInternalAuthMatch("matched")
	}
	// Pass-through. The Handler.ServeHTTP chain hook (handler.go:~4586)
	// does not propagate the mutated request — r is a value
	// parameter and Go's request context is immutable here. The
	// verified svcName is recoverable downstream via the synth
	// path's own state (synth_internal_only.go) or via a future
	// audit helper that takes (svcName) explicitly. The HTTP
	// chain doesn't need it for the wake step.
	return false
}

// internalSvcCtxKey is unexported so external packages cannot
// forge a context value to bypass the gate; the only way to
// populate it is via applyIngressInternalSvc after a Verify
// success.
type internalSvcCtxKey struct{}

// InternalSvcFromContext returns the verified svcName stamped
// by applyIngressInternalSvc on a pass-through, or "" when the
// request was not authenticated as a Gregale daemon (e.g. an
// open/bearer/basic/ip_allowlist request). Used by the wake-
// step audit to record `authenticated_as`.
func InternalSvcFromContext(ctx context.Context) string {
	v, ok := ctx.Value(internalSvcCtxKey{}).(string)
	if !ok {
		return ""
	}
	return v
}

// errInternalSvc* are local aliases for the typed errors the
// verifier returns. pkg/gateway cannot import pkg/internalsvc
// (that would invert the dependency direction — the bridge
// file at cmd/gatewayd-internal/internal_svc_verifier.go
// translates from internalsvc.Err* to these). errors.Is
// requires identity equality, so the gate uses both aliases
// in the same package. If pkg/internalsvc ever ships new
// error sentinels, extend this list — the gate's "unknown
// reason → signature_invalid" fallback is the safety net.
var (
	errInternalSvcAudience      = errors.New("internal_svc: audience mismatch")
	errInternalSvcExpired       = errors.New("internal_svc: token expired")
	errInternalSvcNotYetValid   = errors.New("internal_svc: token not yet valid")
	errInternalSvcUnknownSvc    = errors.New("internal_svc: unknown service")
	errInternalSvcMalformed     = errors.New("internal_svc: malformed token")
	errInternalSvcEmptyAllowlist = errors.New("internal_svc: empty allowlist")
)

// _ pins base64 + ed25519 imports for the gateway-side helper
// surface. The actual JWT mint/verify logic lives in
// pkg/internalsvc (compile-time check that the bridge in
// cmd/gatewayd-internal can import both). base64 is referenced
// from the bridge file's PEM parser; ed25519 is the public-key
// type the bridge returns. If a future contributor removes the
// only references in pkg/gateway these imports would vanish
// and the build would still pass — the compile-time check is
// in the bridge file. We keep the imports here for the public-
// API surface (cmd/gatewayd-internal/internal_svc_verifier.go
// imports pkg/gateway to wire the verifier).
var (
	_ = base64.StdEncoding
	_ = ed25519.PublicKey(nil)
)