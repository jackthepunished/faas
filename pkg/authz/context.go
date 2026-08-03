package authz

import (
	"context"
	"net/http"

	authmw "github.com/onebox-faas/faas/pkg/auth/middleware"
	"github.com/onebox-faas/faas/pkg/state"
)

// principalFromCtx is a thin re-export over authmw.PrincipalFrom so
// pkg/authz callers don't need to import the middleware package
// directly (and so we have one place to evolve the principal shape
// — the org role, in particular, may grow more fields in PR 6/7).
//
// Returns ok=false when RequireSession didn't run — same fail-closed
// contract as the underlying accessor.
func principalFromCtx(ctx context.Context) (state.Account, *state.APIKey, *state.OrgMembership, bool) {
	// principalFromCtx receives context.Context but pkg/auth/middleware
	// keys on *http.Request. The cmd/apid route table ensures
	// every LoadOrg-bearing handler is HTTP, so we cast through a
	// sentinel ctx value that LoadOrgWithResolver stamps before
	// calling AuthorizeOrgAction. See context.go::fromContextRequest
	// + loadorg.go::handleLoadOrg's ctx stamping.
	if r, ok := ctx.Value(httpReqCtxKey{}).(*http.Request); ok && r != nil {
		return authmw.PrincipalFrom(r)
	}
	return state.Account{}, nil, nil, false
}

// MembershipFrom is the org-aware accessor for the rest of the
// codebase. Returns the active-org membership stamped by
// LoadOrgWithResolver, or (nil, false) for requests that did not
// pass through LoadOrg (the pre-PR-5 default) or that came in without
// an X-Active-Org / ?org= hint.
//
// Authoritative for the 9 org actions defined in action.go — call
// AuthorizeOrgAction(ctx, action, audit) instead of branching on
// the membership role directly. The OrgRole read accessor exists
// for code paths that genuinely need to render the role (the
// /v1/orgs/me handler in cmd/apid/handlers_org_me.go).
func MembershipFrom(r *http.Request) (*state.OrgMembership, bool) {
	_, _, mem, ok := authmw.PrincipalFrom(r)
	if !ok || mem == nil {
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
func WithActiveOrg(ctx context.Context, mem *state.OrgMembership) context.Context {
	if r, ok := ctx.Value(httpReqCtxKey{}).(*http.Request); ok && r != nil {
		acct, key, _, ok := authmw.PrincipalFrom(r)
		if !ok {
			return ctx
		}
		return authmw.WithPrincipal(ctx, acct, key, mem)
	}
	return ctx
}

// WithRequestOnContext is the LoadOrgWithResolver-side helper that
// wraps the request as a ctx value so principalFromCtx can recover
// it. Unexported — only LoadOrgWithResolver calls it.
//
// The seam exists so AuthorizeOrgAction(r.Context(), action, audit)
// can read the same principal as the handler without taking
// *http.Request directly. PR 7 may add a parallel gRPC chain.
func WithRequestOnContext(ctx context.Context, r *http.Request) context.Context {
	return context.WithValue(ctx, httpReqCtxKey{}, r)
}

// httpReqCtxKey is the typed ctx guard for principalFromCtx and
// WithActiveOrg. Distinct from any pkg/auth/middleware key (which
// uses iota) and from pkg/session/mfa keys (which use struct{}) so
// a future insert in either doesn't collide.
type httpReqCtxKey struct{}
