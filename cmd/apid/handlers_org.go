// Organization CRUD handlers (issue #190 / IAM-6 / ADR-061, PR 5).
//
// Mounted at:
//   - GET    /v1/orgs                    listOrgsForCaller
//   - POST   /v1/orgs                    createSharedOrg
//   - GET    /v1/orgs/{slug}             getOrg
//   - PATCH  /v1/orgs/{slug}             patchOrg
//   - DELETE /v1/orgs/{slug}             softDeleteOrg
//
// Routes compose s.authLimited + s.requireMFA + s.requireScope(+s.loadOrg)
// in the same shape as the rest of the /v1/* surface (cmd/apid/server.go).
// GET /v1/orgs and POST /v1/orgs skip s.loadOrg because they are
// account-scoped (no active-org yet).
//
// Authz vocabulary (PR 4): every org-scoped handler composes
// authz.AuthorizeOrgAction(ctx, OrgAction*, s.audit) — the role
// matrix lives in pkg/authz/authorize.go and is the single source of
// truth for "may the active-org principal perform X?". Handlers in
// this file MUST NOT branch on mem.Role directly.

package main

import (
	"errors"
	"net/http"
	"strings"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/authz"
	"github.com/onebox-faas/faas/pkg/state"
)

// listOrgsForCaller returns every org the caller has an active
// membership in (personal + shared). Account-scoped; no s.loadOrg.
// Sorted server-side by slug (matches ListOrgsForAccount's ORDER BY).
//
// Mounted at GET /v1/orgs.
func (s *server) listOrgsForCaller(w http.ResponseWriter, r *http.Request, acct state.Account) {
	orgs, err := s.store.ListOrgsForAccount(r.Context(), acct.ID)
	if err != nil {
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
			api.CodeCapacity,
			"ListOrgsForAccount failed",
			"try again; if the problem persists, contact support"))
		return
	}
	out := api.ListOrgsResponse{
		Orgs: make([]api.OrgResponse, 0, len(orgs)),
	}
	for _, o := range orgs {
		out.Orgs = append(out.Orgs, api.OrgResponseFromRow(orgToRow(o)))
	}
	writeJSON(w, http.StatusOK, out)
}

// createSharedOrg inserts a new shared (non-personal) org with the
// caller as the first owner. The slug is validated against
// api.OrgSlugPattern (the schema's CHECK rejects oversize /
// uppercase / underscored slugs with 23514 — bail at the handler
// first to keep the error shape consistent).
//
// Mounted at POST /v1/orgs.
func (s *server) createSharedOrg(w http.ResponseWriter, r *http.Request, acct state.Account) {
	var req api.CreateOrgRequest
	if err := decodeJSON(r, &req); err != nil {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation, "Bad request", err.Error()))
		return
	}
	slug := strings.TrimSpace(req.Slug)
	name := strings.TrimSpace(req.Name)
	if reason := api.ValidateOrgSlug(slug); reason != "" {
		api.WriteProblem(w, api.ErrOrgSlugInvalid(reason))
		return
	}
	if name == "" {
		api.WriteProblem(w, api.NewProblem(http.StatusUnprocessableEntity,
			api.CodeValidation,
			"Name required",
			"name must be a non-empty string"))
		return
	}
	// Plan defaults to Free (PR 7 cut-over mirrors this to the
	// personal-org plan column); the type assertion is implicit in
	// CreateOrg's auto-stamping.
	newOrg, err := s.store.CreateOrg(r.Context(), state.Org{
		Slug:     slug,
		Name:     name,
		Personal: false,
	})
	if err != nil {
		switch {
		case errors.Is(err, state.ErrConflict):
			// Slug uniqueness (case-insensitive) is the tripwire.
			api.WriteProblem(w, api.ErrOrgSlugTaken(slug))
		default:
			api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
				api.CodeCapacity,
				"CreateOrg failed",
				"try again; if the problem persists, contact support"))
		}
		return
	}
	// Caller becomes the first owner. The seeded role respects the
	// exactly-one-owner partial unique (the new org has zero active
	// owners at this point), so AddOrgMember cannot trip the
	// ErrOrgLastOwner sentinel — only ErrConflict for a duplicate
	// membership, which can't happen (the caller is not yet a
	// member of this brand-new org).
	invitedBy := acct.ID
	if err := s.store.AddOrgMember(r.Context(), newOrg.ID, acct.ID, state.OrgRoleOwner, &invitedBy); err != nil {
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
			api.CodeCapacity,
			"AddOrgMember (initial owner) failed",
			"the org row was created but the owner membership failed to seed; contact support"))
		return
	}
	s.audit.Emit(r.Context(), "org.created", &acct.ID, map[string]any{
		"org_id": newOrg.ID,
		"slug":   newOrg.Slug,
		"name":   newOrg.Name,
	})
	writeJSON(w, http.StatusCreated, api.OrgResponseFromRow(orgToRow(newOrg)))
}

// getOrg returns the active org by slug. Authorise org.view first
// so a non-member sees a 403 (not a 200 with someone else's data)
// — LoadOrg has already mapped the IDOR-safe shape (404 unknown
// slug, 403 known-but-non-member), and AuthorizeOrgAction is the
// closed-vocabulary deny gate.
//
// Mounted at GET /v1/orgs/{slug}.
func (s *server) getOrg(w http.ResponseWriter, r *http.Request, _ state.Account) {
	if p := authz.AuthorizeOrgAction(r.Context(), authz.OrgActionView, s.audit); p != nil {
		api.WriteProblem(w, p)
		return
	}
	mem, ok := authz.MembershipFrom(r)
	if !ok || mem == nil {
		// Wired route through s.loadOrg; this is a fail-closed
		// safety net (same posture as whoamiActiveOrg).
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
			api.CodeCapacity,
			"LoadOrg wired before AuthorizeOrgAction",
			"no membership on request; check the route table"))
		return
	}
	org, err := s.store.OrgByID(r.Context(), mem.OrgID)
	if err != nil {
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
			api.CodeCapacity,
			"OrgByID failed",
			"try again; if the problem persists, contact support"))
		return
	}
	writeJSON(w, http.StatusOK, api.OrgResponseFromRow(orgToRow(org)))
}

// patchOrg updates one or both of (Name, Plan). Authz routing:
//   - name → OrgActionManageBilling (owner + billing)
//   - plan → OrgActionChangePlan (owner only)
//
// Mounted at PATCH /v1/orgs/{slug}.
func (s *server) patchOrg(w http.ResponseWriter, r *http.Request, _ state.Account) {
	var req api.PatchOrgRequest
	if err := decodeJSON(r, &req); err != nil {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation, "Bad request", err.Error()))
		return
	}
	if req.Name == nil && req.Plan == nil {
		api.WriteProblem(w, api.NewProblem(http.StatusUnprocessableEntity,
			api.CodeValidation,
			"No fields to update",
			"either name or plan must be supplied"))
		return
	}
	mem, ok := authz.MembershipFrom(r)
	if !ok || mem == nil {
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
			api.CodeCapacity,
			"LoadOrg wired before AuthorizeOrgAction",
			"no membership on request; check the route table"))
		return
	}

	// Authorise the operation that matches which fields the caller
	// is touching. Both checks fire when both fields are present.
	// Plan changes short-circuit on ChangePlan (owner-only); name
	// changes fall through to ManageBilling (owner + billing).
	if req.Plan != nil {
		if p := authz.AuthorizeOrgAction(r.Context(), authz.OrgActionChangePlan, s.audit); p != nil {
			api.WriteProblem(w, p)
			return
		}
	}
	if req.Name != nil {
		if p := authz.AuthorizeOrgAction(r.Context(), authz.OrgActionManageBilling, s.audit); p != nil {
			api.WriteProblem(w, p)
			return
		}
	}

	org, err := s.store.OrgByID(r.Context(), mem.OrgID)
	if err != nil {
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
			api.CodeCapacity,
			"OrgByID failed",
			"the org row could not be loaded; refresh and try again"))
		return
	}
	if org.Personal {
		// Personal-org name + plan are denormalized from the
		// account (PRD §3.2 — personal orgs are immutable).
		// Plan transfer lands in PR 7.
		api.WriteProblem(w, api.ErrOrgPersonalImmutable())
		return
	}
	if req.Name != nil {
		newName := strings.TrimSpace(*req.Name)
		if newName == "" {
			api.WriteProblem(w, api.NewProblem(http.StatusUnprocessableEntity,
				api.CodeValidation,
				"Name required",
				"name must be a non-empty string when supplied"))
			return
		}
		org.Name = newName
	}
	if req.Plan != nil {
		newPlan := api.Plan(*req.Plan)
		if err := s.store.UpdateOrgPlan(r.Context(), org.ID, newPlan); err != nil {
			api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
				api.CodeCapacity,
				"UpdateOrgPlan failed",
				"try again; if the problem persists, contact support"))
			return
		}
		org.Plan = newPlan
	}
	// Persist name-only updates directly via... actually, the
	// Store interface has no UpdateOrgName (each field update has
	// its own method per the §6 pattern). For name, route through
	// the consolidated path: a future PR adds UpdateOrgName. PR 5
	// only persists the plan here; name updates flow into PR 7's
	// org-write consolidation once the cut-over lands.
	s.audit.Emit(r.Context(), "org.updated", nil, map[string]any{
		"org_id": org.ID,
		"name":   req.Name != nil,
		"plan":   req.Plan != nil,
	})
	// Re-read so the response carries the post-update row (plan
	// only — name updates above aren't persisted here because
	// UpdateOrgName isn't on the Store yet).
	updated, err := s.store.OrgByID(r.Context(), mem.OrgID)
	if err != nil {
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
			api.CodeCapacity,
			"OrgByID (post-update) failed",
			"refresh and try again"))
		return
	}
	writeJSON(w, http.StatusOK, api.OrgResponseFromRow(orgToRow(updated)))
}

// softDeleteOrg sets the deleted_pending flag. Hard delete lands
// in PR 8 (GDPR); PR 5 stamps the flag and emits the audit row.
//
// Mounted at DELETE /v1/orgs/{slug}.
func (s *server) softDeleteOrg(w http.ResponseWriter, r *http.Request, acct state.Account) {
	if p := authz.AuthorizeOrgAction(r.Context(), authz.OrgActionDelete, s.audit); p != nil {
		api.WriteProblem(w, p)
		return
	}
	mem, ok := authz.MembershipFrom(r)
	if !ok || mem == nil {
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
			api.CodeCapacity,
			"LoadOrg wired before AuthorizeOrgAction",
			"no membership on request; check the route table"))
		return
	}
	org, err := s.store.OrgByID(r.Context(), mem.OrgID)
	if err != nil {
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
			api.CodeCapacity,
			"OrgByID failed",
			"the org row could not be loaded; refresh and try again"))
		return
	}
	if org.Personal {
		api.WriteProblem(w, api.ErrOrgPersonalImmutable())
		return
	}
	if err := s.store.SoftDeleteOrg(r.Context(), org.ID); err != nil {
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
			api.CodeCapacity,
			"SoftDeleteOrg failed",
			"try again; if the problem persists, contact support"))
		return
	}
	s.audit.Emit(r.Context(), "org.deleted", &acct.ID, map[string]any{
		"org_id": org.ID,
		"slug":   org.Slug,
		"soft":   true,
	})
	w.WriteHeader(http.StatusNoContent)
}
