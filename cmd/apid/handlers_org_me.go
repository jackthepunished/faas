// Organization-aware whoami. Issue #190 / IAM-6 / ADR-061, PR 4.
//
// GET /v1/orgs/me returns the caller's currently-active org plus the
// membership role, or {"org": null} when no X-Active-Org / ?org=
// hint was supplied. The endpoint is the load-bearing seam that
// exercises pkg/authz.LoadOrg end-to-end before PR 5 mounts the
// full org CRUD surface.
//
// Wire-shape (response body):
//
//	{
//	  "org": {
//	    "id": "<uuid>",
//	    "slug": "u-<12hex>",
//	    "name": "Personal",
//	    "personal": true,
//	    "role": "owner"
//	  }
//	}
//
// or {"org": null}. The endpoint is NOT documented in api/openapi.yaml
// for PR 4 — PR 5 adds the spec coverage alongside the rest of the
// /v1/orgs/{slug} surface. The dashboard does not yet render this
// shape; PR 5+ adds the "Personal | Acme Inc." switcher.
//
// The handler MUST NOT reject a missing header — that's the
// passthrough case the rest of the platform depends on (every
// pre-PR-5 route stays account-scoped). 4xx comes only from a
// known-but-unauthorised org (403) or a malformed/empty slug (404).
package main

import (
	"encoding/json"
	"net/http"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/authz"
	"github.com/onebox-faas/faas/pkg/state"
)

// orgMeResponse is the wire shape of GET /v1/orgs/me. The struct is
// defined locally (not in pkg/api/dto.go) because the endpoint is
// load-bearing for PR 4 only and PR 5 will introduce the canonical
// /v1/orgs/{slug}/... DTO surface that supersedes it.
type orgMeResponse struct {
	Org *orgMeOrg `json:"org"`
}

type orgMeOrg struct {
	ID       string        `json:"id"`
	Slug     string        `json:"slug"`
	Name     string        `json:"name"`
	Personal bool          `json:"personal"`
	Role     state.OrgRole `json:"role"`
}

// whoamiActiveOrg is the handler mounted at GET /v1/orgs/me. It
// reads the membership stamped by s.loadOrg (issue #190 / IAM-6 /
// ADR-061, PR 4) and renders the response.
//
// Behaviour:
//   - no membership on ctx → {"org": null} (passthrough — LoadOrg
//     stamps the membership only when X-Active-Org / ?org= was set).
//   - membership present → fetch the org by id (the membership's
//     OrgID) so the response carries the slug + name + personal
//     flag. Membership fetch happens via s.store so we don't need
//     to round-trip through the resolver; OrgByID is a single SELECT.
//
// Errors:
//   - 500 CodeCapacity if OrgByID fails for any reason other than
//     state.ErrNotFound (which would imply a stale membership row —
//     a real bug that surfaces in audit).
func (s *server) whoamiActiveOrg(w http.ResponseWriter, r *http.Request, _ state.Account) {
	mem, ok := authz.MembershipFrom(r)
	if !ok || mem == nil {
		// Apply the same {"org": null} passthrough that LoadOrg
		// uses when neither the X-Active-Org header nor the ?org=
		// query is set. Tested by TestE2E_LoadOrg_HeaderMiss
		// (cmd/e2e/load_org_e2e_test.go).
		//
		// The mem == nil check is load-bearing: the principal's
		// Membership slot is nil-stamped by RequireSession
		// (pkg/auth/middleware/middleware.go:466, 531) and only
		// mutated by LoadOrg on a successful resolve. MembershipFrom
		// returns ok=true when the principal exists but the
		// membership slot is nil — i.e. before LoadOrg ran. We
		// treat that as the passthrough case so a header-miss
		// request doesn't deref mem.OrgID.
		writeOrgMeJSON(w, orgMeResponse{Org: nil})
		return
	}
	org, err := s.store.OrgByID(r.Context(), mem.OrgID)
	if err != nil {
		// Defensive: a stale membership row (membership exists
		// but the org row vanished) surfaces here. PR 8 handles
		// GDPR/purge; for PR 4 we treat it as a 500 + audit.
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
			api.CodeCapacity,
			"Active org unavailable",
			"the active org's row could not be loaded; refresh and try again"))
		return
	}
	writeOrgMeJSON(w, orgMeResponse{Org: &orgMeOrg{
		ID:       org.ID,
		Slug:     org.Slug,
		Name:     org.Name,
		Personal: org.Personal,
		Role:     mem.Role,
	}})
}

// writeOrgMeJSON serialises the response with the API's standard
// content type. Errors here are unrecoverable (the body already
// started) so we log and best-effort the write.
func writeOrgMeJSON(w http.ResponseWriter, body orgMeResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		// Body already committed; nothing else to do.
		_ = err
	}
}
