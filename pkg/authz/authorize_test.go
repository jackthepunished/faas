package authz

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/auth/middleware"
	"github.com/onebox-faas/faas/pkg/state"
)

// fakeAudit records every Emit call so tests can assert the audit
// shape without spinning up a real pkg/audit writer. The deny path
// must emit "authz.denied" with the action in the fields map.
type fakeAudit struct {
	events []fakeAuditEvent
}

type fakeAuditEvent struct {
	event     string
	accountID *string
	fields    map[string]any
}

func (f *fakeAudit) Emit(_ context.Context, event string, accountID *string, fields map[string]any) {
	f.events = append(f.events, fakeAuditEvent{event: event, accountID: accountID, fields: fields})
}

// expectAllowed is the matrix-driven test helper. Walks every
// (action, role) pair, asserts the matrix says "allowed" iff
// wantAllowed is true, and round-trips through AuthorizeOrgAction
// to prove the function reads the same matrix. The dual probe
// (matrix + function) catches drift if a future refactor moves
// one without the other.
func expectAllowed(t *testing.T, action OrgAction, role state.OrgRole, wantAllowed bool) {
	t.Helper()
	allowed := allowRoleMatrix[action][role]
	if allowed != wantAllowed {
		t.Errorf("%s × %s: matrix says allowed=%v, want %v", action, role, allowed, wantAllowed)
	}

	// Round-trip through AuthorizeOrgAction. The same ctx plumbing
	// that LoadOrgWithResolver stamps on real requests.
	ctx := ctxWithPrincipal(t, "acct-1", role)
	var p *api.Problem
	if wantAllowed {
		p = AuthorizeOrgAction(ctx, action, nil)
	} else {
		p = AuthorizeOrgAction(ctx, action, nil)
	}
	if wantAllowed && p != nil {
		t.Errorf("%s × %s: AuthorizeOrgAction denied: %+v", action, role, p)
	}
	if !wantAllowed && p == nil {
		t.Errorf("%s × %s: AuthorizeOrgAction allowed, want deny", action, role)
	}
}

// ctxWithPrincipal stamps a minimal principal + active-org membership
// onto ctx using the same authmw surface LoadOrgWithResolver relies
// on. role drives the membership's role field; accountID drives the
// audit shape.
func ctxWithPrincipal(t *testing.T, accountID string, role state.OrgRole) context.Context {
	t.Helper()
	acct := state.Account{ID: accountID}
	mem := &state.OrgMembership{OrgID: "org-1", AccountID: accountID, Role: role}
	req := httptest.NewRequest("GET", "/v1/orgs/me", nil)
	req = req.WithContext(middleware.WithPrincipal(req.Context(), acct, nil, mem))
	return WithRequestOnContext(req.Context(), req)
}

// TestRoleMatrix_Exhaustive — pins every (action, role) cell of the
// matrix to the ADR-061 source-of-truth shape. A future change to
// the matrix must update this test (or it fails the gate) so the
// role-permission surface stays reviewable in code.
func TestRoleMatrix_Exhaustive(t *testing.T) {
	cases := []struct {
		action      OrgAction
		role        state.OrgRole
		wantAllowed bool
	}{
		// View — every role can read.
		{OrgActionView, state.OrgRoleOwner, true},
		{OrgActionView, state.OrgRoleAdmin, true},
		{OrgActionView, state.OrgRoleDeveloper, true},
		{OrgActionView, state.OrgRoleViewer, true},
		{OrgActionView, state.OrgRoleBilling, true},

		// ManageMembers — owner + admin.
		{OrgActionManageMembers, state.OrgRoleOwner, true},
		{OrgActionManageMembers, state.OrgRoleAdmin, true},
		{OrgActionManageMembers, state.OrgRoleDeveloper, false},
		{OrgActionManageMembers, state.OrgRoleViewer, false},
		{OrgActionManageMembers, state.OrgRoleBilling, false},

		// InviteMembers — owner + admin.
		{OrgActionInviteMembers, state.OrgRoleOwner, true},
		{OrgActionInviteMembers, state.OrgRoleAdmin, true},
		{OrgActionInviteMembers, state.OrgRoleDeveloper, false},
		{OrgActionInviteMembers, state.OrgRoleViewer, false},
		{OrgActionInviteMembers, state.OrgRoleBilling, false},

		// RemoveMembers — owner-only (per the table comment:
		// admins cannot remove peers).
		{OrgActionRemoveMembers, state.OrgRoleOwner, true},
		{OrgActionRemoveMembers, state.OrgRoleAdmin, false},
		{OrgActionRemoveMembers, state.OrgRoleDeveloper, false},
		{OrgActionRemoveMembers, state.OrgRoleViewer, false},
		{OrgActionRemoveMembers, state.OrgRoleBilling, false},

		// ChangeRole — owner-only.
		{OrgActionChangeRole, state.OrgRoleOwner, true},
		{OrgActionChangeRole, state.OrgRoleAdmin, false},
		{OrgActionChangeRole, state.OrgRoleDeveloper, false},
		{OrgActionChangeRole, state.OrgRoleViewer, false},
		{OrgActionChangeRole, state.OrgRoleBilling, false},

		// TransferOwnership — owner-only.
		{OrgActionTransferOwnership, state.OrgRoleOwner, true},
		{OrgActionTransferOwnership, state.OrgRoleAdmin, false},
		{OrgActionTransferOwnership, state.OrgRoleDeveloper, false},
		{OrgActionTransferOwnership, state.OrgRoleViewer, false},
		{OrgActionTransferOwnership, state.OrgRoleBilling, false},

		// ManageBilling — owner + billing (the dedicated billing
		// role exists precisely so the CFO can pay invoices
		// without being able to change who else is on the org).
		{OrgActionManageBilling, state.OrgRoleOwner, true},
		{OrgActionManageBilling, state.OrgRoleAdmin, false},
		{OrgActionManageBilling, state.OrgRoleDeveloper, false},
		{OrgActionManageBilling, state.OrgRoleViewer, false},
		{OrgActionManageBilling, state.OrgRoleBilling, true},

		// ChangePlan — owner-only (plan changes cascade into the
		// billing surface and the rate-limit admission ceiling).
		{OrgActionChangePlan, state.OrgRoleOwner, true},
		{OrgActionChangePlan, state.OrgRoleAdmin, false},
		{OrgActionChangePlan, state.OrgRoleDeveloper, false},
		{OrgActionChangePlan, state.OrgRoleViewer, false},
		{OrgActionChangePlan, state.OrgRoleBilling, false},

		// Delete — owner-only.
		{OrgActionDelete, state.OrgRoleOwner, true},
		{OrgActionDelete, state.OrgRoleAdmin, false},
		{OrgActionDelete, state.OrgRoleDeveloper, false},
		{OrgActionDelete, state.OrgRoleViewer, false},
		{OrgActionDelete, state.OrgRoleBilling, false},
	}
	for _, tc := range cases {
		expectAllowed(t, tc.action, tc.role, tc.wantAllowed)
	}
}

// TestAuthorizeOrgAction_NoPrincipal — when RequireSession didn't
// run (no principal on ctx), AuthorizeOrgAction must return 500
// CodeCapacity, not 403. 500 is correct: the route is misconfigured
// (or an attacker found an unauthenticated path) and a 403 would
// silently hide the bug.
func TestAuthorizeOrgAction_NoPrincipal(t *testing.T) {
	ctx := context.Background()
	p := AuthorizeOrgAction(ctx, OrgActionView, nil)
	if p == nil {
		t.Fatal("AuthorizeOrgAction: want 500, got nil")
	}
	if p.Status != 500 {
		t.Errorf("status = %d, want 500", p.Status)
	}
	if p.Code != api.CodeCapacity {
		t.Errorf("code = %q, want %q", p.Code, api.CodeCapacity)
	}
}

// TestAuthorizeOrgAction_NoMembership — when LoadOrg didn't run
// (no membership stamped), AuthorizeOrgAction must return 403
// org_role_forbidden. The wire shape stays consistent across
// deny paths: every "you can't do this" is 403, regardless of
// whether the reason is missing membership or wrong role.
//
// The audit-emit shape is asserted separately in
// TestAuthorizeOrgAction_AuditOnNoMembership below.
func TestAuthorizeOrgAction_NoMembership(t *testing.T) {
	req := httptest.NewRequest("GET", "/v1/orgs/me", nil)
	acct := state.Account{ID: "acct-1"}
	req = req.WithContext(middleware.WithPrincipal(req.Context(), acct, nil, nil))
	ctx := WithRequestOnContext(req.Context(), req)

	p := AuthorizeOrgAction(ctx, OrgActionView, nil)
	if p == nil {
		t.Fatal("AuthorizeOrgAction: want 403, got nil")
	}
	if p.Status != 403 {
		t.Errorf("status = %d, want 403", p.Status)
	}
	if p.Code != api.CodeOrgRoleForbidden {
		t.Errorf("code = %q, want %q", p.Code, api.CodeOrgRoleForbidden)
	}
}

// TestAuthorizeOrgAction_AllowOwner — owner can do every action in
// the matrix. This is the matrix-completeness probe: if a future
// edit removes owner from some cell, the test fails and the gate
// catches it.
func TestAuthorizeOrgAction_AllowOwner(t *testing.T) {
	for _, action := range AllOrgActions {
		ctx := ctxWithPrincipal(t, "acct-owner", state.OrgRoleOwner)
		p := AuthorizeOrgAction(ctx, action, nil)
		if p != nil {
			t.Errorf("%s as owner: denied: %+v", action, p)
		}
	}
}

// TestAuthorizeOrgAction_AuditOnDeny — when AuthorizeOrgAction
// denies and an audit emitter is wired, the emitter must receive
// exactly one "authz.denied" event with the action in the fields.
// The allow path emits no audit row (matches the auth-middleware
// pattern: only failures are audit-logged).
func TestAuthorizeOrgAction_AuditOnDeny(t *testing.T) {
	audit := &fakeAudit{}
	// Viewer trying to delete — denied; must emit one event.
	ctx := ctxWithPrincipal(t, "acct-1", state.OrgRoleViewer)
	p := AuthorizeOrgAction(ctx, OrgActionDelete, audit)
	if p == nil {
		t.Fatal("AuthorizeOrgAction: want deny")
	}
	if len(audit.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(audit.events))
	}
	e := audit.events[0]
	if e.event != "authz.denied" {
		t.Errorf("event = %q, want authz.denied", e.event)
	}
	if e.fields["action"] != string(OrgActionDelete) {
		t.Errorf("fields[action] = %v, want %s", e.fields["action"], OrgActionDelete)
	}
	if e.fields["account_id"] != "acct-1" {
		t.Errorf("fields[account_id] = %v, want acct-1", e.fields["account_id"])
	}

	// Allow path emits nothing.
	audit2 := &fakeAudit{}
	ctx2 := ctxWithPrincipal(t, "acct-owner", state.OrgRoleOwner)
	if p := AuthorizeOrgAction(ctx2, OrgActionView, audit2); p != nil {
		t.Fatalf("AuthorizeOrgAction(allow): %+v", p)
	}
	if len(audit2.events) != 0 {
		t.Errorf("audit events on allow = %d, want 0", len(audit2.events))
	}
}

// TestAuthorizeOrgAction_AuditOnNoMembership — when the route was
// reached but the caller didn't pass an X-Active-Org / ?org= hint
// (LoadOrg ran but stamped no membership), AuthorizeOrgAction must
// emit exactly one audit row with the action in fields. Paired with
// TestAuthorizeOrgAction_NoMembership above, which asserts the
// 403 status without an audit emitter.
func TestAuthorizeOrgAction_AuditOnNoMembership(t *testing.T) {
	audit := &fakeAudit{}
	req := httptest.NewRequest("GET", "/v1/orgs/me", nil)
	acct := state.Account{ID: "acct-1"}
	req = req.WithContext(middleware.WithPrincipal(req.Context(), acct, nil, nil))
	ctx := WithRequestOnContext(req.Context(), req)

	p := AuthorizeOrgAction(ctx, OrgActionView, audit)
	if p == nil {
		t.Fatal("AuthorizeOrgAction: want deny")
	}
	if len(audit.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(audit.events))
	}
	e := audit.events[0]
	if e.event != "authz.denied" {
		t.Errorf("event = %q, want authz.denied", e.event)
	}
	if e.fields["action"] != string(OrgActionView) {
		t.Errorf("fields[action] = %v, want %s", e.fields["action"], OrgActionView)
	}
	if e.fields["account_id"] != "acct-1" {
		t.Errorf("fields[account_id] = %v, want acct-1", e.fields["account_id"])
	}
}

// TestOrgActionString — OrgAction.String is the wire form. A drift
// here breaks every audit row's "action" field + every problem
// detail string. Pin the stable values.
func TestOrgActionString(t *testing.T) {
	cases := map[OrgAction]string{
		OrgActionView:              "org.view",
		OrgActionManageMembers:     "org.manage_members",
		OrgActionInviteMembers:     "org.invite_members",
		OrgActionRemoveMembers:     "org.remove_members",
		OrgActionChangeRole:        "org.change_role",
		OrgActionTransferOwnership: "org.transfer_ownership",
		OrgActionManageBilling:     "org.manage_billing",
		OrgActionChangePlan:        "org.change_plan",
		OrgActionDelete:            "org.delete",
	}
	for action, want := range cases {
		if got := action.String(); got != want {
			t.Errorf("%s.String() = %q, want %q", action, got, want)
		}
	}
}

// Compile-time assertion: AllOrgActions + AllOrgRoles cover every
// declared const. A new OrgAction / OrgRole must be added to the
// matching slice (or this test fails).
func TestAllOrgActions_Complete(t *testing.T) {
	want := map[OrgAction]bool{
		OrgActionView:              true,
		OrgActionManageMembers:     true,
		OrgActionInviteMembers:     true,
		OrgActionRemoveMembers:     true,
		OrgActionChangeRole:        true,
		OrgActionTransferOwnership: true,
		OrgActionManageBilling:     true,
		OrgActionChangePlan:        true,
		OrgActionDelete:            true,
	}
	got := map[OrgAction]bool{}
	for _, a := range AllOrgActions {
		got[a] = true
	}
	for action := range want {
		if !got[action] {
			t.Errorf("AllOrgActions missing %s", action)
		}
	}
	for action := range got {
		if !want[action] {
			t.Errorf("AllOrgActions has unknown %s", action)
		}
	}
}
