// Organization invitation + ownership-transfer handlers
// (issue #190 / IAM-6 / ADR-061, PR 5).
//
// Mounted at:
//   - GET  /v1/invitations/{token}            peekInvitation
//   - POST /v1/orgs/{slug}/transfer_ownership transferOrgOwnership
//
// The invitation-create handler (POST /v1/orgs/{slug}/members) lives
// in handlers_org_members.go (create-only — returns the plaintext
// token ONCE). The accept handler (POST /v1/invitations/{token}/accept)
// is PR 8 (issue #190 PR-8 — SSO + GDPR bundle). PR 5 ships:
//   - The peek surface (read-only, helps the dashboard render "you're
//     invited to Acme Inc. as developer" without consuming the token).
//   - The ownership-transfer surface (the data-layer seam from
//     pkg/state::TransferOrgOwnership).

package main

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/authz"
	"github.com/onebox-faas/faas/pkg/state"
)

// peekInvitation is the read-only GET /v1/invitations/{token}
// surface. It does NOT consume the token — the accept path is PR 8.
//
// Mounted without s.loadOrg: the invitee doesn't have an
// X-Active-Org yet (the invitation IS how they get an active-org).
// The token-bearing query is the authentication seam. The route is
// still s.auth-bounded so the caller has a valid session or API
// key, but no org-scoped RBAC fires (the principal doesn't yet
// belong to the target org).
//
// Mounted at GET /v1/invitations/{token}.
func (s *server) peekInvitation(w http.ResponseWriter, r *http.Request, _ state.Account) {
	token := r.PathValue("token")
	if token == "" {
		api.WriteProblem(w, api.ErrOrgInvitationInvalid())
		return
	}
	plaintext, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		// Malformed token → 410 (don't leak whether a token was
		// ever minted; same posture as the LoadOrg 404 path on
		// unknown slugs).
		api.WriteProblem(w, api.ErrOrgInvitationInvalid())
		return
	}
	hash := sha256.Sum256(plaintext)
	inv, err := s.store.OrgInvitationByTokenHash(r.Context(), hash[:])
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			api.WriteProblem(w, api.ErrOrgInvitationInvalid())
			return
		}
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
			api.CodeCapacity,
			"OrgInvitationByTokenHash failed",
			"try again; if the problem persists, contact support"))
		return
	}
	org, err := s.store.OrgByID(r.Context(), inv.OrgID)
	if err != nil {
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
			api.CodeCapacity,
			"OrgByID failed",
			"the invitation's org row vanished; contact support"))
		return
	}
	now := time.Now()
	status := api.DeriveOrgInvitationStatus(inv.ConsumedAt, inv.RevokedAt, inv.ExpiresAt, now)
	if status == "consumed" || status == "revoked" || status == "expired" {
		// Collapse every terminal-state token (consumed /
		// revoked / expired) onto a single ErrOrgInvitationInvalid
		// so an attacker who has a leaked token cannot enumerate
		// which rows are still live by the code returned. The
		// Expired constructor is reserved for the PR 8 accept
		// flow where the caller is the legitimate invitee and
		// needs to know "this token is past its TTL" so the
		// dashboard can render a "request a new invite" CTA.
		api.WriteProblem(w, api.ErrOrgInvitationInvalid())
		return
	}
	row := invitationToRow(inv, org.Slug, now)
	writeJSON(w, http.StatusOK, api.OrgInvitationResponseFromRow(row))
}

// transferOrgOwnership atomically promotes the target account to
// owner and demotes the caller to admin via Store.TransferOrgOwnership.
//
// Mounted at POST /v1/orgs/{slug}/transfer_ownership.
func (s *server) transferOrgOwnership(w http.ResponseWriter, r *http.Request, acct state.Account) {
	if !s.requireOrgAction(w, r, authz.OrgActionTransferOwnership) {
		return
	}
	mem, ok := s.requireMembership(w, r)
	if !ok {
		return
	}
	var req api.TransferOwnershipRequest
	if err := decodeJSON(r, &req); err != nil {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation, "Bad request", err.Error()))
		return
	}
	newOwnerID := strings.TrimSpace(req.NewOwnerAccountID)
	if newOwnerID == "" {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
			"Missing new_owner_account_id", "new_owner_account_id must be a non-empty string"))
		return
	}
	if newOwnerID == mem.AccountID {
		// Self-transfer is the canonical "already the owner"
		// case; refuse explicitly so the wire shape stays
		// consistent (the store would otherwise return
		// ErrOrgLastOwner).
		api.WriteProblem(w, api.NewProblem(http.StatusConflict, api.CodeValidation,
			"Already owner", "the caller is already the owner; no transfer needed"))
		return
	}
	if err := s.store.TransferOrgOwnership(r.Context(), mem.OrgID, mem.AccountID, newOwnerID); err != nil {
		switch {
		case errors.Is(err, state.ErrNotFound):
			api.WriteProblem(w, api.NewProblem(http.StatusNotFound, api.CodeNotFound,
				"Member not found", "the new owner is not an active member of this org"))
		case errors.Is(err, state.ErrOrgLastOwner):
			api.WriteProblem(w, api.ErrOrgLastOwner())
		default:
			api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
				api.CodeCapacity,
				"TransferOrgOwnership failed",
				"try again; if the problem persists, contact support"))
		}
		return
	}
	s.audit.Emit(r.Context(), "org.ownership_transferred", &acct.ID, map[string]any{
		"org_id":       mem.OrgID,
		"from_account": acct.ID,
		"to_account":   newOwnerID,
	})
	updated, ok := s.rehydrateOrg(r.Context(), w, mem)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, api.OrgResponseFromRow(orgToRow(updated)))
}
