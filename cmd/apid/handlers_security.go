// handlers_security.go — apid admin-gated handlers for per-app
// signature-enforcement controls (issue #472 / ADR-054).
//
// Route (registered in cmd/apid/server.go::handler with the admin+MFA
// chain — see the mount block):
//
//	PATCH /v1/apps/{slug}/security  → patchAppSecurity
//
// Why a dedicated endpoint (instead of folding RequireSigned into
// updateApp):
//
//   - Signature enforcement is an operator control, NOT a customer
//     knob. A customer who can PATCH require_signed=true on their own
//     app can immediately circumvent the gate they're turning on
//     (they can pre-stage the trusted_signers table however they
//     want). The dedicated endpoint restricts the toggle to the
//     admin scope (ScopesAdminOnly), matching the trusted-signer
//     surface below.
//
//   - The wire is intentionally narrow: only RequireSigned is
//     settable today. Future admin-only knobs (e.g. an allow-list
//     of trusted registries, a customer-side key-rotation policy)
//     land here so the PATCH /v1/apps/{slug} endpoint stays a
//     customer-safe surface.
//
//   - The mount-time chain (authLimited → requireMFA →
//     requireScope(api.ScopesAdminOnly...)) mirrors
//     `PATCH /v1/account/plan` at server.go:516 — same posture, same
//     idempotency wrapper, same problem-code surface.

package main

import (
	"fmt"
	"net/http"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// AppSecurityRequest is the body of PATCH /v1/apps/{slug}/security.
// (Alias for api.AppSecurityRequest — defined here only so the
// handler file reads self-contained; the spec-compliance gate
// matches on the api package's DTO.)
//   - See api.AppSecurityRequest.
//
// AppSecurityResponse is the success body of PATCH
// /v1/apps/{slug}/security.
//   - See api.AppSecurityResponse.
//
// patchAppSecurity applies admin-scoped per-app security knobs.
// Today this is just the require_signed toggle; future knobs land
// here so the customer PATCH surface stays admin-free.
//
// Hand-rolled phases (resolve app → validate body → persist →
// notify → audit), not a helper, because the line budget is well
// under the §Conventions 50-line cap and the phase order matters
// for auditing.
//
// Mounted with authLimited → requireMFA → requireScope(ScopesAdminOnly);
// the per-field admin check is therefore redundant (the route is
// already admin-only), but it's documented in the docstring so a
// future caller doesn't accidentally widen the route's scope.
func (s *server) patchAppSecurity(w http.ResponseWriter, r *http.Request, acct state.Account) {
	slug := r.PathValue("slug")
	app, ok := s.loadApp(w, r, acct, slug)
	if !ok {
		return
	}
	var req api.AppSecurityRequest
	if err := decodeJSON(r, &req); err != nil {
		api.WriteProblem(w, api.ErrValidation("invalid JSON body"))
		return
	}
	// nil = no field set; treat as a no-op rather than 400 so an
	// empty-body probe from the dashboard's "Save" button (with no
	// fields changed) doesn't fail. A future field here that has
	// required values can override this branch.
	if req.RequireSigned == nil {
		writeJSON(w, http.StatusOK, api.AppSecurityResponse{RequireSigned: app.RequireSigned})
		return
	}
	updated, err := s.store.UpdateApp(ctx(r), app.ID, state.UpdateAppParams{
		RequireSigned:    req.RequireSigned,
		SetRequireSigned: true,
	})
	if err != nil {
		api.WriteProblem(w, api.ErrCapacity("could not update app security"))
		return
	}
	// pg_notify on the deployment-changed channel — imaged's verify
	// path reads apps.require_signed at buildImageLayer time, so
	// flipping the flag takes effect on the NEXT deploy (no
	// in-flight deploy re-evaluates). Same posture as the other
	// app-config toggles.
	_ = s.notif.Notify(ctx(r), "app_changed", fmt.Sprintf(
		`{"kind":"security","app_id":"%s","require_signed":%t}`, app.ID, updated.RequireSigned))
	// Audit — IAM-4 (issue #291) shape: record what the admin
	// altered, old vs new. The `app.security_updated` kind is the
	// distinct taxonomy entry so the audit-log panel can filter
	// signature-related config changes separately from generic
	// app.updated.
	s.audit.Emit(ctx(r), "app.security_updated", &acct.ID, map[string]any{
		"app_id":      updated.ID,
		"slug":        updated.Slug,
		"old_require": app.RequireSigned,
		"new_require": updated.RequireSigned,
	})
	writeJSON(w, http.StatusOK, api.AppSecurityResponse{RequireSigned: updated.RequireSigned})
}
