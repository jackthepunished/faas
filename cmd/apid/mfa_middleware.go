// MFA gate (IAM-2, issue #186).
//
// requireMFA is the session-cookie-side companion to requireScope.
// It runs after s.auth has stamped the verified principal, and
// reads the mfa-pending flag off the session envelope via a separate
// context value (withMFAPending). The flag is true when the cookie
// was issued by a login path on an account that is
// mfa_required && !mfa_enrolled; the cookie-stamp path in s.auth
// (cmd/apid/server.go, the cookie branch) is the only writer.
//
// Two non-trivial decisions:
//
//   1. The mfa-pending flag is a *distinct* context key from
//      principalCtxKey. principalCtxKey is an iota-typed const; a
//      future PR inserting a value in the middle would silently
//      shift the int. The MFA flag lives on its own string-typed
//      key to make the wire-shape intent explicit and to make the
//      `mfaPendingFrom(r) → (false, false)` fallback detectable
//      from a miswire.
//
//   2. The MFA allowlist is path-prefixed against r.URL.Path so
//      the dashboard's /v1/account whoami can render the "MFA
//      required" prompt without itself being gated. Every other
//      session-cookie route 403s CodeMFARequired. API keys bypass
//      the gate (mfaPendingFrom returns false/ok=false → key path
//      bypass) per the IAM-2 design decision in the plan.

package main

import (
	"context"
	"net/http"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// mfaPendingCtxKey is the unexported type-guard around the
// withMFAPending context value. Distinct from principalCtxKey
// (which is iota-typed in server.go) so a future insert in that
// const block doesn't silently collide.
type mfaPendingCtxKey struct{}

// withMFAPending decorates the request context for the requireMFA
// wrapper. Stamped by the cookie branch of s.auth (server.go: the
// `c, err := r.Cookie(sessionCookie)` block); not stamped by the
// bearer branch because API keys bypass MFA per the IAM-2 design
// decision 3. A missing value (`mfaPendingFrom` returns
// (false, false)) is the deliberate signal for "bearer key —
// bypass".
func withMFAPending(ctx context.Context, pending bool) context.Context {
	return context.WithValue(ctx, mfaPendingCtxKey{}, pending)
}

// mfaPendingFrom returns the mfa-pending flag stamped by s.auth's
// cookie branch. (false, false) means the principal was a bearer
// key (no MFA on the table) OR the routes were wired without
// s.auth (a test miswire). Both cases bypass the gate.
func mfaPendingFrom(r *http.Request) (bool, bool) {
	v := r.Context().Value(mfaPendingCtxKey{})
	if v == nil {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

// mfaAllowlist is the set of paths that stay reachable while the
// cookie is mfa_pending. The intent is: the dashboard can still
// render the "MFA required" prompt, and the customer can complete
// enrollment / step-up / recovery / disable without first
// satisfying MFA on a different route.
//
//   - /v1/account                — whoami (lets the dashboard know
//     which account it's prompting).
//   - /v1/account/mfa/enroll     — start enrollment.
//   - /v1/account/mfa/confirm    — finish enrollment.
//   - /v1/account/mfa/verify     — step up an mfa_pending session.
//   - /v1/account/mfa/recover    — burn a recovery code.
//   - /v1/account/mfa/disable    — opt out (if mfa_required is
//     false).
//
// Exact-path match: the route table uses Go 1.22+ `method
// /path` form so r.URL.Path is the full pattern, no trailing
// wildcards. Adding a new MFA sub-route means adding it here.
// The /v1/auth/* cookie-issue paths are NOT in the allowlist —
// they are not gated by s.auth at all (they live on the
// dashboardAuthChain), so they never reach requireMFA.
var mfaAllowlist = []string{
	"/v1/account",
	"/v1/account/mfa/enroll",
	"/v1/account/mfa/confirm",
	"/v1/account/mfa/verify",
	"/v1/account/mfa/recover",
	"/v1/account/mfa/disable",
}

// isMFAAllowlisted is the predicate requireMFA calls on
// r.URL.Path. Returns true for any path in mfaAllowlist. False
// on every other session-cookie route, which 403s CodeMFARequired.
func isMFAAllowlisted(path string) bool {
	for _, p := range mfaAllowlist {
		if p == path {
			return true
		}
	}
	return false
}

// requireMFA gates every session-cookie route behind the
// mfa-pending flag. It is a no-op on bearer-key requests
// (mfaPendingFrom returns false/ok=false → key path bypass).
// Composes as
//
//	s.authLimited(s.requireMFA(s.requireScope(...)(s.handler)))
//
// because the wrap order is auth → mfa → scope → idempotent →
// handler: auth stashes the principal + mfa-pending flag, mfa
// short-circuits to 403 if the flag is true and the path isn't
// allowlisted, scope enforces the per-route authorization,
// idempotent caches the response, and the handler runs only when
// all four pass.
//
// Returning a 403 (not a redirect) is deliberate: the dashboard
// reads the RFC 7807 problem document and renders the "complete
// MFA to continue" prompt in-place. A redirect would force the
// browser to round-trip the dashboard through the gateway and
// would lose the original request method + body.
//
// RequireMFA returns 403 on the allowlisted paths too — the
// dashboard can still render the prompt, but the customer still
// can't reach /v1/apps while pending. The /v1/account whoami is
// the only handler that needs to run *and* render the prompt.
func (s *server) requireMFA(next accountHandler) accountHandler {
	return func(w http.ResponseWriter, r *http.Request, acct state.Account) {
		pending, ok := mfaPendingFrom(r)
		if !ok {
			// Bearer-key principal or unwired test: bypass.
			next(w, r, acct)
			return
		}
		if !pending {
			// Cookie was issued by a login path that already
			// cleared MFA, OR by /v1/account/mfa/verify after a
			// successful TOTP step-up. No gate.
			next(w, r, acct)
			return
		}
		if isMFAAllowlisted(r.URL.Path) {
			// Render the prompt / drive the MFA flow.
			next(w, r, acct)
			return
		}
		api.WriteProblem(w, api.NewProblem(http.StatusForbidden,
			api.CodeMFARequired, "MFA required",
			"complete /v1/account/mfa/enroll or /v1/account/mfa/verify to access this route"))
		// Best-effort audit: a session cookie repeatedly hitting a
		// gated route is the same threat signal as a 401 on the
		// login path. The dashboard reads these rows to surface
		// "session expired / MFA required" rather than crashing.
		if s.audit != nil {
			s.audit.Emit(r.Context(), "auth.mfa_gate_hit", &acct.ID, map[string]any{
				"path":   r.URL.Path,
				"method": r.Method,
			})
		}
	}
}

// mfaEnrollRequired is the predicate the login handlers check to
// decide whether to stamp MfaPending=true on the new session
// cookie. Returns true iff the account has the policy flag set
// AND has not yet enrolled. The inverse is "the customer has
// either cleared MFA or never been required to".
//
// Used by handlers_auth_*.go and the OAuth callbacks. Kept as a
// free function (not a method on Account) so the auth handlers
// can call it inline without exposing the predicate outside
// cmd/apid.
func mfaEnrollRequired(acct state.Account) bool {
	return acct.MFARequired && !acct.MFAEnrolled()
}
