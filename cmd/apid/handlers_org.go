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
	"context"
	"errors"
	"fmt"
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
// caller as the first owner. Slug validation runs at the handler so
// the wire shape stays consistent (the schema's 23514 tripwire
// would otherwise produce a raw 500).
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
			api.CodeValidation, "Name required", "name must be a non-empty string"))
		return
	}
	// Plan defaults to Free; CreateOrg stamps it. The caller seeds
	// the exactly-one-owner partial unique (zero owners at insert
	// time), so AddOrgMember cannot trip ErrOrgLastOwner.
	newOrg, err := s.store.CreateOrg(r.Context(), state.Org{Slug: slug, Name: name})
	if err != nil {
		if errors.Is(err, state.ErrConflict) {
			api.WriteProblem(w, api.ErrOrgSlugTaken(slug))
		} else {
			api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
				api.CodeCapacity, "CreateOrg failed",
				"try again; if the problem persists, contact support"))
		}
		return
	}
	// Note (IAM-6 / ADR-061 PR 2): we deliberately do NOT call
	// enforceMemberCap here — the initial-owner seed is the
	// first active membership on a brand-new org, and Free's
	// fail-closed 0/0 cap would refuse it. The cap is a
	// per-add gate for subsequent members; the personal-org
	// path (which is immutable and never reaches this handler)
	// is unaffected. The store-side cap check inside AddOrgMember
	// is the defence-in-depth back-stop — it also reads
	// 0/0 for Free but only trips when active >= limit, so
	// the initial seed (active=0) passes cleanly.
	invitedBy := acct.ID
	if err := s.store.AddOrgMember(r.Context(), newOrg.ID, acct.ID, state.OrgRoleOwner, &invitedBy); err != nil {
		if errors.Is(err, state.ErrOrgMemberCapExceeded) {
			limit := newOrg.Plan.OrgMembersMax()
			api.WriteProblem(w, api.ErrOrgMemberCapExceeded(limit, limit))
			return
		}
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
			api.CodeCapacity, "AddOrgMember (initial owner) failed",
			"the org row was created but the owner membership failed to seed; contact support"))
		return
	}
	s.audit.Emit(r.Context(), "org.created", &acct.ID, map[string]any{
		"org_id": newOrg.ID, "slug": newOrg.Slug, "name": newOrg.Name,
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
	if !s.requireOrgAction(w, r, authz.OrgActionView) {
		return
	}
	mem, ok := s.requireMembership(w, r)
	if !ok {
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
			api.CodeValidation, "No fields to update",
			"either name or plan must be supplied"))
		return
	}
	mem, ok := s.requireMembership(w, r)
	if !ok {
		return
	}
	if !s.authorizeOrgPatchFields(w, r, req) {
		return
	}
	newName, newPlan, ok := s.resolveOrgPatchFields(w, req)
	if !ok {
		return
	}
	org, ok := s.loadMutableOrgByMembership(r.Context(), w, mem)
	if !ok {
		return
	}
	if !s.applyOrgFieldUpdates(r.Context(), w, org.ID, req, newName, newPlan) {
		return
	}
	s.audit.Emit(r.Context(), "org.updated", nil, map[string]any{
		"org_id": org.ID, "name": req.Name != nil, "plan": req.Plan != nil,
	})
	updated, ok := s.rehydrateOrg(r.Context(), w, mem)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, api.OrgResponseFromRow(orgToRow(updated)))
}

// authorizeOrgPatchFields authorises the operation(s) that match
// which fields the caller is touching. Plan changes short-circuit
// on ChangePlan (owner-only); name changes fall through to
// ManageBilling (owner + billing). Both checks fire when both
// fields are present in the request body.
func (s *server) authorizeOrgPatchFields(w http.ResponseWriter, r *http.Request, req api.PatchOrgRequest) bool {
	if req.Plan != nil {
		if p := authz.AuthorizeOrgAction(r.Context(), authz.OrgActionChangePlan, s.audit); p != nil {
			api.WriteProblem(w, p)
			return false
		}
	}
	if req.Name != nil {
		if p := authz.AuthorizeOrgAction(r.Context(), authz.OrgActionManageBilling, s.audit); p != nil {
			api.WriteProblem(w, p)
			return false
		}
	}
	return true
}

// resolveOrgPatchFields validates + trims the per-field values
// before any Store call (validation failures short-circuit before
// a doomed UpdateOrgName). Returns newName, newPlan, true on
// success; writes the Problem + returns zero values + false on
// any failure.
func (s *server) resolveOrgPatchFields(w http.ResponseWriter, req api.PatchOrgRequest) (string, api.Plan, bool) {
	var newName string
	var newPlan api.Plan
	if req.Name != nil {
		newName = strings.TrimSpace(*req.Name)
		if newName == "" {
			api.WriteProblem(w, api.NewProblem(http.StatusUnprocessableEntity,
				api.CodeValidation, "Name required",
				"name must be a non-empty string when supplied"))
			return "", "", false
		}
	}
	if req.Plan != nil {
		newPlan = api.Plan(strings.TrimSpace(*req.Plan))
		if !isKnownPlan(newPlan) {
			// Reuse ErrOrgSlugInvalid's wire shape for the closed
			// enum: 422 org_slug_invalid with the closed set named
			// in the detail (we don't add a new wire code; the
			// catalogue is closed at PR 1).
			api.WriteProblem(w, api.ErrOrgSlugInvalid(
				fmt.Sprintf("plan %q is not in the closed set %v", string(newPlan), api.Plans)))
			return "", "", false
		}
	}
	return newName, newPlan, true
}

// applyOrgFieldUpdates persists the per-field changes for PATCH
// /v1/orgs/{slug}. Each field has its own Store method per the §6
// pattern (one SQL UPDATE per field, no consolidated multi-column
// write); both stamp updated_at = now() so the wire shape's
// UpdatedAt is monotonic per row. Returns false (and writes the
// Problem) on the first Store failure.
func (s *server) applyOrgFieldUpdates(ctx context.Context, w http.ResponseWriter, orgID string, req api.PatchOrgRequest, newName string, newPlan api.Plan) bool {
	if req.Name != nil {
		if err := s.store.UpdateOrgName(ctx, orgID, newName); err != nil {
			api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
				api.CodeCapacity, "UpdateOrgName failed",
				"try again; if the problem persists, contact support"))
			return false
		}
	}
	if req.Plan != nil {
		if err := s.store.UpdateOrgPlan(ctx, orgID, newPlan); err != nil {
			api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
				api.CodeCapacity, "UpdateOrgPlan failed",
				"try again; if the problem persists, contact support"))
			return false
		}
	}
	return true
}

// isKnownPlan is the closed-enum membership check for the PATCH
// /v1/orgs/{slug} body. Mirrors the slug validator's posture:
// the wire shape carries a free string and the handler rejects
// anything outside the closed set with ErrOrgSlugInvalid's wire
// shape (the catalogue is closed at PR 1; adding a new plan is a
// separate concern).
func isKnownPlan(p api.Plan) bool {
	for _, k := range api.Plans {
		if k == p {
			return true
		}
	}
	return false
}

// softDeleteOrg sets the deleted_pending flag. Hard delete lands
// in PR 8 (GDPR); PR 5 stamps the flag and emits the audit row.
//
// Mounted at DELETE /v1/orgs/{slug}.
func (s *server) softDeleteOrg(w http.ResponseWriter, r *http.Request, acct state.Account) {
	if !s.requireOrgAction(w, r, authz.OrgActionDelete) {
		return
	}
	mem, ok := s.requireMembership(w, r)
	if !ok {
		return
	}
	org, ok := s.loadMutableOrgByMembership(r.Context(), w, mem)
	if !ok {
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
