// Context plumbing for the authenticated request. The three values
// RequireSession stamps into r.Context() (via withPrincipal,
// withSession, withMFAPending) are read back via AccountFromContext,
// SessionFromContext, MFAPendingFrom.
//
// The principal type is unexported because pkg/auth wants to
// retain the freedom to migrate the ctx key across a guard later.
// Callers that need to know "is this principal admin?" compose with
// RequireScope (which honours the session-cookie implicit-admin
// rule) instead of reaching into the principal directly.
package middleware

import (
	"context"
	"net/http"

	"github.com/onebox-faas/faas/pkg/state"
)

// principal is the authenticated caller. Key is nil when the caller
// authenticated via the dashboard session cookie (in which case
// RequireScope treats the caller as implicitly admin). Mirrors
// cmd/apid/server.go:1239.
type principal struct {
	Acct state.Account
	Key  *state.APIKey
}

type ctxKey int

const principalCtxKey ctxKey = iota

// principalFrom returns the principal stashed in r.Context() by
// RequireSession. Returns ok=false if RequireSession did not run
// (e.g. tests that wire a handler directly to httptest).
func principalFrom(r *http.Request) (principal, bool) {
	v := r.Context().Value(principalCtxKey)
	if v == nil {
		return principal{}, false
	}
	p, ok := v.(principal)
	return p, ok
}

// withPrincipal returns a context carrying the principal.
// Unexported — only RequireSession stamps; only principalFrom +
// AccountFromContext read.
func withPrincipal(ctx context.Context, p principal) context.Context {
	return context.WithValue(ctx, principalCtxKey, p)
}

// AccountFromContext returns the (Account, APIKey) pair stashed by
// RequireSession. Returns ok=false if the request never went
// through RequireSession (the middleware wasn't wired) — callers
// should treat this as a wiring bug and fail closed (RequireScope
// emits 500 CodeCapacity; cmd/apid routes have the same fail-closed
// behaviour today).
//
// Key is nil for session-cookie auth; the implicit-admin rule lives
// inside RequireScope, NOT here, so AccountFromContext surfaces
// the raw authenticated shape. Callers that need the
// "session = admin" truth must compose with RequireScope or
// duplicate its rule.
func AccountFromContext(r *http.Request) (state.Account, *state.APIKey, bool) {
	p, ok := principalFrom(r)
	if !ok {
		return state.Account{}, nil, false
	}
	return p.Acct, p.Key, true
}

// WithPrincipal stamps a principal onto ctx. Exported so tests
// (and any future reverse-proxy shim that pre-authenticates
// before the request reaches the daemon) can build a ctx that
// RequireScope will read via AccountFromContext.
//
// Production code MUST use RequireSession — this setter exists
// for the auth-skip path (load tests, integration tests with
// pre-minted identities) and is the only correct way to bypass
// the bearer/session dance without breaking RequireScope's
// fail-closed contract.
func WithPrincipal(ctx context.Context, acct state.Account, key *state.APIKey) context.Context {
	return withPrincipal(ctx, principal{Acct: acct, Key: key})
}

// --- session-cookie plumbing --------------------------------------------

// sessionCtxKey holds the state.Session row RequireSession stamps
// after the live-row cross-check passes. Distinct from
// principalCtxKey (which is iota-typed) so a future insert in that
// const block doesn't silently collide.
type sessionCtxKey struct{}

// withSession stamps the live state.Session onto the request
// context. /v1/auth/sessions/{id} handlers read it back via
// SessionFromContext to know which sid to revoke without
// re-querying.
func withSession(ctx context.Context, sess state.Session) context.Context {
	return context.WithValue(ctx, sessionCtxKey{}, sess)
}

// SessionFromContext returns the live state.Session stamped by
// RequireSession's cookie branch. ok=false means the request
// didn't authenticate via cookie (bearer-key paths).
func SessionFromContext(r *http.Request) (*state.Session, bool) {
	v := r.Context().Value(sessionCtxKey{})
	if v == nil {
		return nil, false
	}
	s, ok := v.(state.Session)
	if !ok {
		return nil, false
	}
	return &s, true
}

// --- mfa-pending plumbing -----------------------------------------------

// mfaPendingCtxKey is the typed ctx guard. Distinct from
// principalCtxKey (iota-typed) so a future insert doesn't collide.
// Distinct from sessionCtxKey (typed struct{} already, but a
// different one) so the MFA flag is a wire-shape intent signal, not
// a "session row carries this somewhere" side-channel.
type mfaPendingCtxKey struct{}

// withMFAPending decorates the request context with the
// mfa-pending flag. The session-cookie branch of RequireSession
// stamps this; the bearer branch deliberately does NOT (API keys
// bypass MFA per IAM-2 design decision 3).
func withMFAPending(ctx context.Context, pending bool) context.Context {
	return context.WithValue(ctx, mfaPendingCtxKey{}, pending)
}

// MFAPendingFrom returns the flag stamped by WithMFAPending. The
// bool is false when the stamp is absent (bearer auth path). The
// ok return distinguishes "not stamped" from "stamped false" so
// RequireMFA can short-circuit correctly on each branch.
//
// Exported so tests + future callers (PR-2 gatewayd AppLogsHandler)
// can inspect without going through WithMFAPending.
func MFAPendingFrom(r *http.Request) (bool, bool) {
	v := r.Context().Value(mfaPendingCtxKey{})
	if v == nil {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

// WithMFAPending stamps the mfa-pending flag onto ctx. Exported
// for the same test-seam reason as WithPrincipal: tests need to
// build a context that RequireMFA will read via MFAPendingFrom.
//
// Production code MUST stamp this via RequireSession's
// session-cookie branch (the only writer). A direct call here
// without going through RequireSession would let a request
// bypass the bearer → 402-on-past-due check that RequireSession
// also performs.
func WithMFAPending(ctx context.Context, pending bool) context.Context {
	return withMFAPending(ctx, pending)
}
