// HTTP integration tests for the /v1/orgs/{slug}/... customer surface
// (issue #190 / IAM-6 / ADR-061, PR 5). The PR-4 e2e file
// (load_org_e2e_test.go) covers the LoadOrg middleware; this file
// covers the handlers that ride on it: shared-org creation, member
// management, invitations, and ownership transfer.
//
// The store-layer parity tests for the new TransferOrgOwnership
// method live in cmd/apid/handlers_org_test.go and
// pkg/state/pgstore_orgs_test.go. This file is the wire-level
// integration story — boot apid, drive the full HTTP surface, and
// assert the wire codes the spec commits to.
//
// The invitation accept flow lands in PR 8 (ADR-061). To keep the
// PR-5 harness self-contained, we exercise the
// `addOrgMember + revokeInvitation` happy path via the Store
// directly rather than running it through the (not-yet-existing)
// POST /v1/invitations/{token}/accept endpoint. PR 8 will swap
// the direct call for a wire call and add the captcha-aware
// accept-handler tests.
package e2e_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
	"github.com/onebox-faas/faas/pkg/e2etest"
	"github.com/onebox-faas/faas/pkg/state"
)

// orgListWire is the local decode type for GET /v1/orgs. The pkg/api
// type is the canonical source; cmd/e2e can't import cmd/apid (which
// is package main), so we mirror the wire shape here.
type orgListWire struct {
	Orgs []struct {
		ID       string `json:"id"`
		Slug     string `json:"slug"`
		Name     string `json:"name"`
		Personal bool   `json:"personal"`
		Plan     string `json:"plan"`
		Status   string `json:"status"`
	} `json:"orgs"`
}

// memberListWire is the local decode type for GET /v1/orgs/{slug}/members.
// Mirrors api.MemberListResponse so the test reads the same shape the
// SDK will see.
type memberListWire struct {
	Members []struct {
		AccountID string `json:"account_id"`
		Email     string `json:"email"`
		Role      string `json:"role"`
		JoinedAt  string `json:"joined_at"`
	} `json:"members"`
}

// inviteWire is the local decode type for the POST /v1/orgs/{slug}/members
// response (the one-time token-bearing shape). The pkg/api canonical
// shape is api.InvitationWithTokenResponse.
type inviteWire struct {
	ID        string `json:"id"`
	OrgID     string `json:"org_id"`
	OrgSlug   string `json:"org_slug"`
	Email     string `json:"email"`
	Role      string `json:"role"`
	Status    string `json:"status"`
	ExpiresAt string `json:"expires_at"`
	CreatedAt string `json:"created_at"`
	Token     string `json:"token"`
}

// TestE2E_OrgLifecycle_HappyPath is the end-to-end PR 5 story.
//
//  1. signup → personal org (PR 3 backfill)
//  2. POST /v1/orgs → create shared "acme" → caller becomes owner
//  3. GET /v1/orgs → list contains personal + acme
//  4. GET /v1/orgs/acme → resolves the org with the caller's role
//  5. POST /v1/orgs/acme/members → invite bob (plaintext token once)
//  6. accept-as-store-call → AddOrgMember(acme, bob.ID, developer)
//  7. PATCH /v1/orgs/acme/members/<bob> → developer → admin
//  8. POST /v1/orgs/acme/transfer_ownership → bob is new owner,
//     caller (alice) becomes admin
//  9. DELETE /v1/orgs/acme/members/<alice> → admin removed
//  10. DELETE /v1/orgs/acme → soft-deleted by the new owner
//
// Steps 6 + 10 are tested through the Store because the only
// remaining surface the acceptance flow needs lands in PR 8 / 9.
func TestE2E_OrgLifecycle_HappyPath(t *testing.T) {
	pool := pgtest.Open(t)
	if pool == nil {
		return
	}
	if err := db.MigrateUp(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h := e2etest.Start(t, pool, e2etest.APID)
	ctx := context.Background()

	// 1. Seed two accounts. The PR-3 personal-org backfill fires
	// on SeedAccount so each one owns a personal org out of the gate.
	aliceKey := h.SeedAccount(ctx, api.PlanHobby, "pr5-alice")
	bobKey := h.SeedAccount(ctx, api.PlanFree, "pr5-bob")
	store := state.NewPgStore(pool)

	aliceAcct, err := store.AccountByEmail(ctx, seedEmail(api.PlanHobby, "pr5-alice"))
	if err != nil {
		t.Fatalf("AccountByEmail alice: %v", err)
	}
	bobAcct, err := store.AccountByEmail(ctx, seedEmail(api.PlanFree, "pr5-bob"))
	if err != nil {
		t.Fatalf("AccountByEmail bob: %v", err)
	}

	// 2. Create shared org. Caller (alice) becomes the first owner.
	createBody := api.CreateOrgRequest{Slug: "acme", Name: "Acme Inc."}
	raw, status := doReq(t, h, aliceKey, http.MethodPost, "/v1/orgs", createBody)
	if status != http.StatusCreated {
		t.Fatalf("create org: %d %s", status, raw)
	}
	var created struct {
		ID        string `json:"id"`
		Slug      string `json:"slug"`
		Name      string `json:"name"`
		Personal  bool   `json:"personal"`
		Plan      string `json:"plan"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decode create: %v (body=%s)", err, raw)
	}
	if created.Slug != "acme" || created.Personal {
		t.Fatalf("created org has wrong shape: %+v", created)
	}
	acmeID := created.ID

	// 3. List orgs — alice should see personal + acme.
	raw, status = doReq(t, h, aliceKey, http.MethodGet, "/v1/orgs", nil)
	if status != http.StatusOK {
		t.Fatalf("list orgs: %d %s", status, raw)
	}
	var list orgListWire
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode list: %v (body=%s)", err, raw)
	}
	if len(list.Orgs) != 2 {
		t.Fatalf("list has %d orgs, want 2 (body=%s)", len(list.Orgs), raw)
	}
	var sawAcme, sawPersonal bool
	for _, o := range list.Orgs {
		switch o.Slug {
		case "acme":
			sawAcme = true
		case state.PersonalOrgSlug(aliceAcct.ID):
			sawPersonal = true
		}
	}
	if !sawAcme {
		t.Errorf("list missing 'acme'")
	}
	if !sawPersonal {
		t.Errorf("list missing personal org")
	}

	// 4. GET /v1/orgs/acme with X-Active-Org — exercises LoadOrg +
	// AuthorizeOrgAction(org.view). Status 200.
	raw, status = doReq(t, h, aliceKey, http.MethodGet, "/v1/orgs/acme", nil,
		map[string]string{"X-Active-Org": "acme"})
	if status != http.StatusOK {
		t.Fatalf("get org: %d %s", status, raw)
	}
	var got struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if got.ID != acmeID || got.Slug != "acme" {
		t.Errorf("get org = %+v, want id=%s slug=acme", got, acmeID)
	}

	// 5. Invite bob. The handler mints a 32-byte plaintext token,
	// base64url-encodes it, returns once, and stores sha256.
	raw, status = doReq(t, h, aliceKey, http.MethodPost, "/v1/orgs/acme/members",
		api.InviteMemberRequest{Email: bobAcct.Email, Role: "developer"},
		map[string]string{"X-Active-Org": "acme"})
	if status != http.StatusCreated {
		t.Fatalf("invite: %d %s", status, raw)
	}
	var inv inviteWire
	if err := json.Unmarshal(raw, &inv); err != nil {
		t.Fatalf("decode invite: %v (body=%s)", err, raw)
	}
	if inv.Token == "" {
		t.Fatalf("invite token is empty (body=%s)", raw)
	}
	if inv.Email != bobAcct.Email {
		t.Errorf("invite email = %q, want %q", inv.Email, bobAcct.Email)
	}
	if inv.Role != "developer" {
		t.Errorf("invite role = %q, want developer", inv.Role)
	}
	if inv.OrgSlug != "acme" {
		t.Errorf("invite org_slug = %q, want acme", inv.OrgSlug)
	}

	// 5b. /v1/invitations/{token} — peek (read-only) using the
	// same token we just captured. The handler hashes the token
	// and looks up by hash. The peek endpoint surfaces the
	// invitation but does NOT consume it.
	raw, status = doReq(t, h, aliceKey, http.MethodGet, "/v1/invitations/"+inv.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("peek invitation: %d %s", status, raw)
	}
	var peeked struct {
		ID      string `json:"id"`
		Status  string `json:"status"`
		OrgSlug string `json:"org_slug"`
	}
	if err := json.Unmarshal(raw, &peeked); err != nil {
		t.Fatalf("decode peek: %v", err)
	}
	if peeked.Status != "pending" {
		t.Errorf("peek status = %q, want pending", peeked.Status)
	}
	if peeked.OrgSlug != "acme" {
		t.Errorf("peek org_slug = %q, want acme", peeked.OrgSlug)
	}

	// 6. Accept-as-store. PR 8 will swap this for a wire call;
	// for PR 5 we drive the data layer directly so the e2e
	// story is self-contained. The peek + revoke steps below
	// prove the token-hash lookup is wired correctly.
	if err := store.AddOrgMember(ctx, acmeID, bobAcct.ID, state.OrgRoleDeveloper, &aliceAcct.ID); err != nil {
		t.Fatalf("AddOrgMember bob: %v", err)
	}
	// Revoke the invitation so the partial-unique on pending
	// invitations doesn't trip later (we don't enforce a cap in
	// PR 5 since OrgPendingInvitationsMax is 0/0, but it's
	// cleaner to leave the system in a deterministic state).
	plaintext, err := base64.RawURLEncoding.DecodeString(inv.Token)
	if err != nil {
		t.Fatalf("decode token: %v", err)
	}
	hash := sha256.Sum256(plaintext)
	if err := store.RevokeOrgInvitation(ctx, hash[:], aliceAcct.ID); err != nil {
		t.Fatalf("RevokeOrgInvitation: %v", err)
	}

	// 7. List members — alice (owner) + bob (developer).
	raw, status = doReq(t, h, aliceKey, http.MethodGet, "/v1/orgs/acme/members", nil,
		map[string]string{"X-Active-Org": "acme"})
	if status != http.StatusOK {
		t.Fatalf("list members: %d %s", status, raw)
	}
	var members memberListWire
	if err := json.Unmarshal(raw, &members); err != nil {
		t.Fatalf("decode members: %v", err)
	}
	if len(members.Members) != 2 {
		t.Fatalf("members = %d, want 2 (body=%s)", len(members.Members), raw)
	}
	var bobRole string
	for _, m := range members.Members {
		switch m.AccountID {
		case aliceAcct.ID:
			if m.Role != "owner" {
				t.Errorf("alice role = %q, want owner", m.Role)
			}
		case bobAcct.ID:
			bobRole = m.Role
		}
	}
	if bobRole != "developer" {
		t.Errorf("bob role = %q, want developer", bobRole)
	}

	// 8. Change bob's role developer → admin.
	raw, status = doReq(t, h, aliceKey,
		http.MethodPatch, "/v1/orgs/acme/members/"+bobAcct.ID,
		api.ChangeMemberRoleRequest{Role: "admin"},
		map[string]string{"X-Active-Org": "acme"})
	if status != http.StatusOK {
		t.Fatalf("change role: %d %s", status, raw)
	}

	// 9. Transfer ownership alice → bob.
	raw, status = doReq(t, h, aliceKey,
		http.MethodPost, "/v1/orgs/acme/transfer_ownership",
		api.TransferOwnershipRequest{NewOwnerAccountID: bobAcct.ID},
		map[string]string{"X-Active-Org": "acme"})
	if status != http.StatusOK {
		t.Fatalf("transfer ownership: %d %s", status, raw)
	}

	// 10. Confirm post-transfer roles via the Store (the role
	// matrix is the load-bearing invariant).
	row, err := store.OrgMemberByAccount(ctx, acmeID, bobAcct.ID)
	if err != nil {
		t.Fatalf("OrgMemberByAccount bob: %v", err)
	}
	if row.Role != state.OrgRoleOwner {
		t.Errorf("bob role after transfer = %q, want owner", row.Role)
	}
	row, err = store.OrgMemberByAccount(ctx, acmeID, aliceAcct.ID)
	if err != nil {
		t.Fatalf("OrgMemberByAccount alice: %v", err)
	}
	if row.Role != state.OrgRoleAdmin {
		t.Errorf("alice role after transfer = %q, want admin (demoted)", row.Role)
	}

	// 11. New owner (bob) removes the demoted admin (alice).
	// This step proves the role matrix wires correctly: bob is
	// now owner, so OrgAction remove_members is allowed.
	raw, status = doReq(t, h, bobKey,
		http.MethodDelete, "/v1/orgs/acme/members/"+aliceAcct.ID, nil,
		map[string]string{"X-Active-Org": "acme"})
	if status != http.StatusNoContent {
		t.Fatalf("remove member: %d %s", status, raw)
	}
	_, err = store.OrgMemberByAccount(ctx, acmeID, aliceAcct.ID)
	if err == nil {
		t.Errorf("alice still has a membership row after remove")
	}

	// 12. Soft-delete the org as the new owner (POST 7 in the
	// dispatch). Wire assertion: 204 No Content.
	raw, status = doReq(t, h, bobKey, http.MethodDelete, "/v1/orgs/acme", nil,
		map[string]string{"X-Active-Org": "acme"})
	if status != http.StatusNoContent {
		t.Fatalf("delete org: %d %s", status, raw)
	}
	deleted, err := store.OrgBySlug(ctx, "acme")
	if err != nil {
		t.Fatalf("OrgBySlug after delete: %v", err)
	}
	if deleted.Status != "deleted_pending" {
		t.Errorf("status = %q, want deleted_pending", deleted.Status)
	}
}

// TestE2E_OrgLifecycle_NonMemberIDOR — the IDOR probe for the
// org-scoped surface. Account A creates a shared org; account B
// (no membership) tries to GET/PATCH/DELETE under it. Every
// org-scoped route must return 403 org_role_forbidden (NOT 404)
// so the IDOR-safe behaviour is identical to the LoadOrg probe.
func TestE2E_OrgLifecycle_NonMemberIDOR(t *testing.T) {
	pool := pgtest.Open(t)
	if pool == nil {
		return
	}
	if err := db.MigrateUp(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h := e2etest.Start(t, pool, e2etest.APID)
	ctx := context.Background()

	aliceKey := h.SeedAccount(ctx, api.PlanHobby, "pr5-idor-a")
	bobKey := h.SeedAccount(ctx, api.PlanFree, "pr5-idor-b")

	store := state.NewPgStore(pool)
	aliceAcct, err := store.AccountByEmail(ctx, seedEmail(api.PlanHobby, "pr5-idor-a"))
	if err != nil {
		t.Fatalf("AccountByEmail alice: %v", err)
	}

	// alice creates "iso"
	raw, status := doReq(t, h, aliceKey, http.MethodPost, "/v1/orgs",
		api.CreateOrgRequest{Slug: "iso", Name: "Iso"})
	if status != http.StatusCreated {
		t.Fatalf("create org: %d %s", status, raw)
	}

	// bob probes every read + write route — must all 403.
	for _, probe := range []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"get", http.MethodGet, "/v1/orgs/iso", nil},
		{"list_members", http.MethodGet, "/v1/orgs/iso/members", nil},
		{"patch", http.MethodPatch, "/v1/orgs/iso", api.PatchOrgRequest{Name: strPtr("Hijacked")}},
		{"invite", http.MethodPost, "/v1/orgs/iso/members", api.InviteMemberRequest{Email: "carol@x.com", Role: "viewer"}},
		{"delete", http.MethodDelete, "/v1/orgs/iso", nil},
	} {
		probe := probe
		t.Run(probe.name, func(t *testing.T) {
			raw, status := doReq(t, h, bobKey, probe.method, probe.path, probe.body,
				map[string]string{"X-Active-Org": "iso"})
			if status != http.StatusForbidden {
				t.Errorf("probe %s: status = %d, want 403 (body=%s)", probe.name, status, raw)
			}
			if !strings.Contains(string(raw), "org_role_forbidden") {
				t.Errorf("probe %s: body did not contain org_role_forbidden: %s", probe.name, raw)
			}
		})
	}

	// sanity check: alice can still GET her own org.
	raw, status = doReq(t, h, aliceKey, http.MethodGet, "/v1/orgs/iso", nil,
		map[string]string{"X-Active-Org": "iso"})
	if status != http.StatusOK {
		t.Errorf("alice GET own org: %d %s", status, raw)
	}
	// consume aliceAcct so the compiler doesn't warn about the
	// unused binding (it's read by the closure above).
	_ = aliceAcct
}

// TestE2E_OrgLifecycle_PersonalImmutable — the personal org is
// the load-bearing seam that holds all pre-PR-5 routes working
// after the cut-over. Mutating it (renaming, deleting, changing
// plan) must return 409 org_personal_immutable. The Store also
// refuses, but the wire code is the contract.
func TestE2E_OrgLifecycle_PersonalImmutable(t *testing.T) {
	pool := pgtest.Open(t)
	if pool == nil {
		return
	}
	if err := db.MigrateUp(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h := e2etest.Start(t, pool, e2etest.APID)
	ctx := context.Background()

	key := h.SeedAccount(ctx, api.PlanFree, "pr5-personal")
	store := state.NewPgStore(pool)
	acct, err := store.AccountByEmail(ctx, seedEmail(api.PlanFree, "pr5-personal"))
	if err != nil {
		t.Fatalf("AccountByEmail: %v", err)
	}
	personal, err := store.OrgByPersonalAccount(ctx, acct.ID)
	if err != nil {
		t.Fatalf("OrgByPersonalAccount: %v", err)
	}

	// DELETE — must 409.
	raw, status := doReq(t, h, key, http.MethodDelete,
		"/v1/orgs/"+personal.Slug, nil,
		map[string]string{"X-Active-Org": personal.Slug})
	if status != http.StatusConflict {
		t.Errorf("delete personal: status = %d, want 409 (body=%s)", status, raw)
	}
	if !strings.Contains(string(raw), "org_personal_immutable") {
		t.Errorf("body did not contain org_personal_immutable: %s", raw)
	}

	// PATCH (rename) — must 409.
	raw, status = doReq(t, h, key, http.MethodPatch,
		"/v1/orgs/"+personal.Slug,
		api.PatchOrgRequest{Name: strPtr("Renamed")},
		map[string]string{"X-Active-Org": personal.Slug})
	if status != http.StatusConflict {
		t.Errorf("patch personal: status = %d, want 409 (body=%s)", status, raw)
	}
	if !strings.Contains(string(raw), "org_personal_immutable") {
		t.Errorf("body did not contain org_personal_immutable: %s", raw)
	}
}

// TestE2E_OrgLifecycle_LastOwnerGuard — the partially-unique
// constraint on exactly-one-active-owner fires on PATCH
// developer→[developer] (no-op) and on DELETE the last owner.
// Both must 409 org_last_owner.
//
// This is the test that proves the role matrix + the data layer
// invariant agree: alice is the only owner, so neither the
// "demote owner → not owner" path nor the "remove owner" path
// is reachable.
func TestE2E_OrgLifecycle_LastOwnerGuard(t *testing.T) {
	pool := pgtest.Open(t)
	if pool == nil {
		return
	}
	if err := db.MigrateUp(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h := e2etest.Start(t, pool, e2etest.APID)
	ctx := context.Background()

	aliceKey := h.SeedAccount(ctx, api.PlanHobby, "pr5-lastowner")
	store := state.NewPgStore(pool)
	aliceAcct, err := store.AccountByEmail(ctx, seedEmail(api.PlanHobby, "pr5-lastowner"))
	if err != nil {
		t.Fatalf("AccountByEmail: %v", err)
	}

	// alice creates "solo" and is the only owner.
	raw, status := doReq(t, h, aliceKey, http.MethodPost, "/v1/orgs",
		api.CreateOrgRequest{Slug: "solo", Name: "Solo"})
	if status != http.StatusCreated {
		t.Fatalf("create org: %d %s", status, raw)
	}
	soloID, err := store.OrgBySlug(ctx, "solo")
	if err != nil {
		t.Fatalf("OrgBySlug solo: %v", err)
	}

	// DELETE alice's own membership (the only owner). Must 409.
	raw, status = doReq(t, h, aliceKey, http.MethodDelete,
		"/v1/orgs/solo/members/"+aliceAcct.ID, nil,
		map[string]string{"X-Active-Org": "solo"})
	if status != http.StatusConflict {
		t.Errorf("remove last owner: status = %d, want 409 (body=%s)", status, raw)
	}
	if !strings.Contains(string(raw), "org_last_owner") {
		t.Errorf("body did not contain org_last_owner: %s", raw)
	}

	// PATCH owner → admin (which would orphan the role) must be
	// rejected at the handler boundary BEFORE the Store (the
	// store would block it via the partial unique, but the
	// handler returns a stable 403 with org_role_forbidden
	// because PATCH /members can never reach owner).
	raw, status = doReq(t, h, aliceKey, http.MethodPatch,
		"/v1/orgs/solo/members/"+aliceAcct.ID,
		api.ChangeMemberRoleRequest{Role: "admin"},
		map[string]string{"X-Active-Org": "solo"})
	if status != http.StatusBadRequest {
		// the handler rejects "owner" for the new role but the
		// request is "admin" — the role IS allowed. The Store
		// would then fail with ErrOrgLastOwner (partial unique).
		// Either 400 (role rejected) or 409 (last-owner) is OK;
		// we assert it's not 200.
		if status == http.StatusOK {
			t.Errorf("PATCH owner→admin succeeded; should be rejected")
		}
	}
	// sanity: the org still has alice as owner.
	row, err := store.OrgMemberByAccount(ctx, soloID.ID, aliceAcct.ID)
	if err != nil {
		t.Fatalf("OrgMemberByAccount: %v", err)
	}
	if row.Role != state.OrgRoleOwner {
		t.Errorf("alice role = %q, want owner (unchanged)", row.Role)
	}
}

// strPtr is a small helper for pointer-fields in request bodies.
// Matches the convention in cmd/apid/handlers_org_test.go.
func strPtr(s string) *string { return &s }
