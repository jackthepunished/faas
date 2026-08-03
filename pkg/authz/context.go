package authz

import (
	"context"
	"net/http"

	authmw "github.com/onebox-faas/faas/pkg/auth/middleware"
	"github.com/onebox-faas/faas/pkg/state"
)

// principalFromRequest is the per-request accessor. The principal is
// keyed on *http.Request (pkg/auth/middleware's choice) so we read it
// directly from req. This is the seam a handler must use when it
// already has the request in hand.
//
// Returns ok=false when RequireSession didn't run — same fail-closed
// contract as the underlying accessor.
func principalFromRequest(r *http.Request) (state.Account, *state.APIKey, *state.OrgMembership, bool) {
	return authmw.PrincipalFrom(r)
}

// principalFromCtx is the ctx-only accessor. It exists for callers
// (notably AuthorizeOrgAction) that don't have the request in hand
// but recovered a ctx from one. The seam depends on a prior
// LoadOrgWithResolver call having stamped the request via
// WithRequestOnContext — that stamp is set in handleLoadOrg
// after a successful resolver lookup, so a call BEFORE LoadOrg
// returns (ok=false) and AFTER LoadOrg returns (ok=true) on the
// same request will disagree. handleLoadOrg itself reads via
// principalFromRequest to avoid that round-trip; this function
// is the forward-compat path for code that only has ctx.
//
// PR 7 may add a gRPC chain that doesn't carry *http.Request; that
// caller will need its own principalFromGRPC accessor.
func principalFromCtx(ctx context.Context) (state.Account, *state.APIKey, *state.OrgMembership, bool) {
	if r, ok := ctx.Value(httpReqCtxKey{}).(*http.Request); ok && r != nil {
		return authmw.PrincipalFrom(r)
	}
	return state.Account{}, nil, nil, false
}

// fromContextRequest is the internal helper WithActiveOrg uses to
// recover the request from ctx. Same fail-closed contract as the
// other accessors; behaves as a no-op when called before the request
// has been stamped (returns the original ctx unchanged).
func fromContextRequest(ctx context.Context) (*http.Request, bool) {
	r, ok := ctx.Value(httpReqCtxKey{}).(*http.Request)
	return r, ok && r != nil
}

// MembershipFrom is the org-aware accessor for the rest of the
// codebase. Returns the active-org membership stamped by
// LoadOrgWithResolver, or (nil, false) for requests that did not
// pass through LoadOrg (the pre-PR-5 default) or that came in without
// an X-Active-Org / ?org= hint.
//
// Contract: ok == false means no membership present; ok == true
// means mem != nil. Callers MUST NOT add a defensive `mem == nil`
// check on the ok==true branch.
//
// Authoritative for the 9 org actions defined in action.go — call
// AuthorizeOrgAction(ctx, action, audit) instead of branching on
// the membership role directly. The OrgRole read accessor exists
// for code paths that genuinely need to render the role (the
// /v1/orgs/me handler in cmd/apid/handlers_org_me.go).
func MembershipFrom(r *http.Request) (*state.OrgMembership, bool) {
	_, _, mem, ok := authmw.PrincipalFrom(r)
	if !ok {
		return nil, false
	}
	return mem, true
}

// ActiveOrgID is the tiny sugar over MembershipFrom that returns
// just the org id — useful for handlers that only need to stamp the
// id on outbound RPCs (PR 7 will need it for schedd admission).
func ActiveOrgID(r *http.Request) (string, bool) {
	mem, ok := MembershipFrom(r)
	if !ok {
		return "", false
	}
	return mem.OrgID, true
}

// WithActiveOrg stamps an org membership onto ctx in the principal's
// Membership slot. Exported so PR 5's invitation-accept path can
// promote the freshly-created membership into the principal without
// re-running RequireSession (which would re-issue the session cookie
// and double-bill the AEAD).
//
// Production code MUST go through LoadOrgWithResolver to set the
// membership — this setter is the test-seam / direct-promotion
// counterpart, mirroring the WithPrincipal / RequireSession split
// in pkg/auth/middleware.
//
// mem may be nil (clears an existing membership) — the typical use
// is to REPLACE the membership with one resolved from a different
// org slug mid-request.
//
// Returns ctx unchanged when the request is not stamped on the
// context (LoadOrgWithResolver did not run yet). Callers from tests
// that exercise WithActiveOrg outside LoadOrg must call
// WithRequestOnContext first.
func WithActiveOrg(ctx context.Context, mem *state.OrgMembership) context.Context {
	if r, ok := fromContextRequest(ctx); ok {
		acct, key, _, ok := authmw.PrincipalFrom(r)
		if !ok {
			return ctx
		}
		return authmw.WithPrincipal(ctx, acct, key, mem)
	}
	return ctx
}

// WithRequestOnContext stamps the request as a ctx value so
// AuthorizeOrgAction (and WithActiveOrg) can recover it later.
// Unexported — only LoadOrgWithResolver calls it. The seam exists
// so AuthorizeOrgAction(r.Context(), action, audit) can read the
// same principal as the handler without taking *http.Request
// directly. PR 7 may add a parallel gRPC chain.
func WithRequestOnContext(ctx context.Context, r *http.Request) context.Context {
	return context.WithValue(ctx, httpReqCtxKey{}, r)
}

// httpReqCtxKey is the typed ctx guard for principalFromCtx and
// WithActiveOrg. Distinct from any pkg/auth/middleware key (which
// uses iota) and from pkg/session/mfa keys (which use struct{}) so
// a future insert in either doesn't collide.
type httpReqCtxKey struct{}
