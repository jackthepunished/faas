// Organization-aware whoami. Issue #190 / IAM-6 / ADR-061, PR 4
// + PR 5.
//
// GET /v1/orgs/me returns the caller's currently-active org plus
// the membership role, or {"org": null} when no X-Active-Org / ?org=
// hint was supplied. The endpoint exercises pkg/authz.LoadOrg
// end-to-end and is the load-bearing seam for every org-scoped
// handler that follows.
//
// PR 5 rewrote whoamiActiveOrg to render the canonical
// api.OrgMeResponse (formerly the local orgMeResponse struct),
// placing the wire shape next to the rest of the /v1/orgs/{slug}
// surface in pkg/api/orgs.go.
//
// Wire shape:
//
//	{
//	  "org": {
//	    "id": "<uuid>",
//	    "slug": "u-<12hex>",
//	    "name": "Personal",
//	    "personal": true,
//	    "plan": "free",
//	    "status": "active",
//	    "created_at": "...",
//	    "updated_at": "...",
//	    "role": "owner"
//	  }
//	}
//
// or {"org": null}. The handler MUST NOT reject a missing header —
// that's the passthrough case the rest of the platform depends on
// (every pre-PR-5 route stays account-scoped).
package main

import (
	"net/http"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/authz"
	"github.com/onebox-faas/faas/pkg/state"
)

// whoamiActiveOrg is the handler mounted at GET /v1/orgs/me. It
// reads the membership stamped by s.loadOrg (issue #190 / IAM-6 /
// ADR-061, PR 4) and renders the response.
//
// Behaviour:
//   - no membership on r → {"org": null} (passthrough — LoadOrg
//     stamps the membership only when X-Active-Org / ?org= was
//     set).
//   - membership present → fetch the org by id (the membership's
//     OrgID) so the response carries the slug + name + personal
//     flag. The role field carries the caller's role on the org.
//
// Errors:
//   - 500 CodeCapacity if OrgByID fails (a stale membership row —
//     surfaces in audit).
func (s *server) whoamiActiveOrg(w http.ResponseWriter, r *http.Request, _ state.Account) {
	mem, ok := authz.MembershipFrom(r)
	if !ok || mem == nil {
		// Apply the same {"org": null} passthrough that LoadOrg
		// uses when neither the X-Active-Org header nor the ?org=
		// query is set. Tested by TestE2E_LoadOrg_HeaderMiss
		// (cmd/e2e/load_org_e2e_test.go).
		//
		// The mem == nil check is load-bearing: the principal's
		// Membership slot is nil-stamped by RequireSession and
		// only mutated by LoadOrg on a successful resolve.
		writeJSON(w, http.StatusOK, api.OrgMeResponse{Org: nil})
		return
	}
	org, err := s.store.OrgByID(r.Context(), mem.OrgID)
	if err != nil {
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
			api.CodeCapacity,
			"Active org unavailable",
			"the active org's row could not be loaded; refresh and try again"))
		return
	}
	resp := api.OrgMeResponse{
		Org: &api.OrgWithRole{
			OrgResponse: api.OrgResponseFromRow(orgToRow(org)),
			Role:        string(mem.Role),
		},
	}
	writeJSON(w, http.StatusOK, resp)
}
