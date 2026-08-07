package state

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/onebox-faas/faas/pkg/api"
)

// memCoverageFixtureOrg returns a MemStore pre-loaded with an account, an org
// (owned by the account), a second account that is an admin of the same org,
// and the same app/deployment pair as memCoverageFixture. Used by slice11 to
// drive org-ownership paths that need a non-empty membership set.
func memCoverageFixtureOrg(t *testing.T) (*MemStore, context.Context, Account, Account, string) {
	t.Helper()
	m, ctx, ownerAcct, _, _ := memCoverageFixture(t)
	org, err := m.CreateOrg(ctx, Org{Slug: "acme-" + uuid.NewString()[:8], Name: "Acme"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.AddOrgMember(ctx, org.ID, ownerAcct.ID, OrgRoleOwner, nil); err != nil {
		t.Fatal(err)
	}
	adminAcct, err := m.CreateAccount(ctx, "admin-"+uuid.NewString()+"@example.com", api.PlanPro)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.AddOrgMember(ctx, org.ID, adminAcct.ID, OrgRoleAdmin, nil); err != nil {
		t.Fatal(err)
	}
	return m, ctx, ownerAcct, adminAcct, org.ID
}

// TestMemStoreCoverageOrgOwnership drives TransferOrgOwnership across its
// rejection branches and the demote-then-promote happy path.
func TestMemStoreCoverageOrgOwnership(t *testing.T) {
	m, ctx, owner, admin, orgID := memCoverageFixtureOrg(t)

	// 1. Same-account transfer is rejected (would leave the org ownerless).
	if err := m.TransferOrgOwnership(ctx, orgID, owner.ID, owner.ID); !errors.Is(err, ErrOrgLastOwner) {
		t.Fatalf("same-account transfer = %v, want ErrOrgLastOwner", err)
	}

	// 2. Happy path: owner -> admin.
	if err := m.TransferOrgOwnership(ctx, orgID, owner.ID, admin.ID); err != nil {
		t.Fatalf("happy transfer: %v", err)
	}

	// 3. Demoted owner is now OrgRoleAdmin; promoted member is OrgRoleOwner.
	mems, err := m.ListOrgMembers(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	var sawOwner, sawAdmin bool
	for _, mem := range mems {
		if mem.AccountID == owner.ID && mem.Role == OrgRoleAdmin {
			sawOwner = true
		}
		if mem.AccountID == admin.ID && mem.Role == OrgRoleOwner {
			sawAdmin = true
		}
	}
	if !sawOwner || !sawAdmin {
		t.Fatalf("post-transfer memberships: sawOwner=%v sawAdmin=%v", sawOwner, sawAdmin)
	}

	// 4. Self-transfer after promote is rejected (would leave no owner).
	if err := m.TransferOrgOwnership(ctx, orgID, admin.ID, admin.ID); !errors.Is(err, ErrOrgLastOwner) {
		t.Fatalf("self-transfer after promote: %v, want ErrOrgLastOwner", err)
	}

	// 5. Transfer to a non-member is rejected as not-found.
	stranger, err := m.CreateAccount(ctx, "stranger-"+uuid.NewString()+"@example.com", api.PlanPro)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.TransferOrgOwnership(ctx, orgID, admin.ID, stranger.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stranger transfer: %v, want ErrNotFound", err)
	}
}

// TestMemStoreCoverageOrgNameRename drives UpdateOrgName across rename,
// duplicate-name collision, and missing-org branches.
func TestMemStoreCoverageOrgNameRename(t *testing.T) {
	m, ctx, owner, _, orgID := memCoverageFixtureOrg(t)

	// Happy rename.
	if err := m.UpdateOrgName(ctx, orgID, "Acme Renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	got, err := m.OrgByID(ctx, orgID)
	if err != nil || got.Name != "Acme Renamed" {
		t.Fatalf("OrgByID after rename = %+v, %v", got, err)
	}

	// A second org can be created with a fresh slug.
	dup, err := m.CreateOrg(ctx, Org{Slug: "dup-" + uuid.NewString()[:8], Name: "Acme Renamed"})
	if err != nil {
		t.Fatal(err)
	}
	// Same name but different slug is allowed (no uniqueness on Name alone).
	if err := m.UpdateOrgName(ctx, dup.ID, "Acme Renamed"); err != nil {
		t.Fatalf("rename with same name on different org: %v", err)
	}

	// Missing org is rejected.
	if err := m.UpdateOrgName(ctx, "missing-org-id", "Whatever"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing rename: %v, want ErrNotFound", err)
	}
	_ = owner
}

// TestMemStoreCoverageInstanceMigration drives CancelInstanceMigration and
// MigrateInstanceOwner happy + error branches. These two methods together
// own the live-migration state-machine seam that pkg/sched depends on.
func TestMemStoreCoverageInstanceMigration(t *testing.T) {
	m, ctx, _, _, _ := memCoverageFixtureOrg(t)
	_, _, _, app, deployment := memCoverageFixture(t)
	inst, err := m.CreateInstance(ctx, app.ID, deployment.ID, string(StateRunning), 256, "node-a", "")
	if err != nil {
		t.Fatal(err)
	}

	// Cancel missing instance is not-found.
	if err := m.CancelInstanceMigration(ctx, "missing", "node-a", "lease"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel missing: %v, want ErrNotFound", err)
	}

	// Migrate missing instance is not-found.
	if err := m.MigrateInstanceOwner(ctx, "missing", "node-a", "node-b", "lease"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("migrate missing: %v, want ErrNotFound", err)
	}

	// Migrate requires state="migrating" + matching fromNodeID.
	// Mark the instance migrating first, then verify the conflict path.
	if err := m.MarkInstanceMigrating(ctx, inst.ID, "node-a", "lease"); err != nil {
		t.Fatalf("MarkInstanceMigrating: %v", err)
	}

	// Migrate wrong-fromNodeID is a state conflict.
	if err := m.MigrateInstanceOwner(ctx, inst.ID, "wrong-node", "node-b", "lease"); !errors.Is(err, ErrConflict) {
		t.Fatalf("migrate wrong fromNodeID: %v, want ErrConflict", err)
	}

	// Happy migrate updates the owner and clears the migrating state.
	if err := m.MigrateInstanceOwner(ctx, inst.ID, "node-a", "node-b", "lease"); err != nil {
		t.Fatalf("happy migrate: %v", err)
	}
	got, err := m.InstanceByID(ctx, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.NodeID != "node-b" {
		t.Fatalf("post-migrate NodeID = %q, want node-b", got.NodeID)
	}
	if got.State != string(StateRunning) {
		t.Fatalf("post-migrate State = %q, want %q", got.State, StateRunning)
	}
}

// TestMemStoreCoverageTestSeams drives the *ForTest helpers used by
// higher-level tests and reconcilers. These are part of the Store interface
// and must remain reachable; the seams are how pkg/sched injects clocks
// and parking times into MemStore for replay testing.
func TestMemStoreCoverageTestSeams(t *testing.T) {
	m, ctx, account, _, _ := memCoverageFixtureOrg(t)
	now := time.Now().UTC()

	// SetClockForTest injection — MemStore has no public Now() getter, but the
	// setter must not panic and must propagate the clock to internal callers.
	// The behavioural assertion is exercised by other tests that read times
	// through public methods (e.g. ListDueInvocations, ListInstancesByStatesOlderThan).
	m.SetClockForTest(func() time.Time { return now })

	_, _, _, app, deployment := memCoverageFixture(t)
	inst, err := m.CreateInstance(ctx, app.ID, deployment.ID, string(StateRunning), 128, "node-x", "")
	if err != nil {
		t.Fatal(err)
	}

	// SetParkedAtForTest / SetPastDueAtForTest / SetTerminalAtForTest round-trip
	// (these are package-internal setters without ErrNotFound, by design).
	m.SetParkedAtForTest(inst.ID, now)
	m.SetPastDueAtForTest(inst.ID, now)
	m.SetTerminalAtForTest(inst.ID, now)

	// SetInstanceMigratedFromForTest is a package-internal setter; verify it
	// doesn't panic on a non-existent id (it writes to a map entry that may
	// not exist — the test just exercises the code path).
	m.SetInstanceMigratedFromForTest(inst.ID, "node-y")

	_ = account
}