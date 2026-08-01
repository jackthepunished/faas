// memstore_reassign_test.go — MemStore parity tests for the
// Tier A4 cross-node rebalance surface (PR follow-up to
// PR #509). The pkg/state coverage gate in CI asserts ≥ 70%
// (Makefile test-state-coverage) and the new
// ListOrphanedApps / ReassignAppOwner methods dropped the
// package below the threshold because no test exercised them.
//
// The fixture seeds three apps (one on an active node, one on
// an inactive node, one deleted) plus three compute nodes
// (default-local + a non-default peer active + the same peer
// flipped inactive) and exercises every method's happy +
// at least one negative path. The contract the gate cares
// about is "every public Store method has at least one
// MemStore assertion that runs in CI" — the test bodies below
// intentionally avoid bespoke mocks in favour of the same
// primitives pgstore_test.go uses, so the assertions stay
// bit-for-bit equivalent to the SQL path.

package state

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/onebox-faas/faas/pkg/api"
)

// memRebalanceFixture seeds the Tier A4 surface: one
// app on default-local (active), one app on a peer node
// that the fixture flips inactive (the rebalancer's primary
// input), and one soft-deleted app (must be filtered out).
// Two compute_nodes rows: default-local (active, seeded by
// NewMemStore) + a peer node the test flips to active=false.
//
// Returns the MemStore, the context, the on-active-owner
// app, the on-inactive-owner app, and the soft-deleted app.
func memRebalanceFixture(t *testing.T) (*MemStore, context.Context, App, App, App) {
	t.Helper()
	ctx := context.Background()
	m := NewMemStore()
	account, err := m.CreateAccount(ctx, "rebal-"+uuid.NewString()+"@example.com", api.PlanPro)
	if err != nil {
		t.Fatal(err)
	}

	// Add a peer node (default-local is already seeded).
	peerID := uuid.NewString()
	peerName := "fsn-peer-" + peerID[:8]
	peer, err := m.CreateComputeNode(ctx, ComputeNode{
		Name:               peerName,
		TargetURL:          "tcp://10.0.0.2:7000",
		ScheddTargetURL:    ptrString("tcp://10.0.0.2:7100"),
		VPCPUs:             80,
		MemMB:              28000,
		MaxConcurrency:     10,
		AdmissionCeilingMB: 23800,
		VCPUBudget:         80,
		Active:             true,
	})
	if err != nil {
		t.Fatalf("seed peer compute_node: %v", err)
	}

	// One app on default-local (the active owner). This
	// app must NEVER appear in ListOrphanedApps — its owner
	// is active.
	var defaultLocalID string
	for id, n := range m.computeNodes {
		if n.Name == "default-local" {
			defaultLocalID = id
		}
	}
	if defaultLocalID == "" {
		t.Fatal("default-local row missing from MemStore")
	}
	onActive, err := m.CreateApp(ctx, App{
		AccountID: account.ID, Slug: "on-active-" + uuid.NewString(),
		RAMMB: 128, Status: AppActive, NodeID: defaultLocalID,
	})
	if err != nil {
		t.Fatal(err)
	}

	// One app on the peer (will be the orphan once we flip
	// the peer inactive below).
	onInactive, err := m.CreateApp(ctx, App{
		AccountID: account.ID, Slug: "on-inactive-" + uuid.NewString(),
		RAMMB: 128, Status: AppActive, NodeID: peer.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	// One soft-deleted app on the peer — must NEVER appear
	// in ListOrphanedApps (status filter excludes it).
	deleted, err := m.CreateApp(ctx, App{
		AccountID: account.ID, Slug: "deleted-" + uuid.NewString(),
		RAMMB: 128, Status: AppDeleted, NodeID: peer.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Flip the peer inactive (the rebalancer's trigger).
	if err := m.SetComputeNodeActive(ctx, peer.ID, false); err != nil {
		t.Fatalf("flip peer inactive: %v", err)
	}

	return m, ctx, onActive, onInactive, deleted
}

// ptrString returns *string for the ScheddTargetURL column
// (nullable). Kept local — the existing test fixtures use
// struct literals with raw string values; the ScheddTargetURL
// column needs a pointer for the nullable write path.
func ptrString(s string) *string {
	return &s
}

// TestMemStore_ListOrphanedApps covers the rebalancer's
// primary input set. After the fixture seeds two apps on a
// peer + one on default-local, then flips the peer
// inactive, the active-owned app on default-local is NOT in
// the orphan set; the inactive-owned apps ARE in the orphan
// set (minus the soft-deleted one).
func TestMemStore_ListOrphanedApps(t *testing.T) {
	m, ctx, onActive, onInactive, deleted := memRebalanceFixture(t)

	got, err := m.ListOrphanedApps(ctx, -1, 50)
	if err != nil {
		t.Fatalf("ListOrphanedApps: %v", err)
	}

	// The orphan set contains onInactive (active status, on
	// inactive peer) but NOT onActive (active status, on
	// active default-local) and NOT deleted (status =
	// AppDeleted).
	want := map[string]bool{onInactive.ID: true}
	for _, a := range got {
		if a.ID == deleted.ID {
			t.Errorf("ListOrphanedApps included soft-deleted app %s", deleted.ID)
		}
		if a.ID == onActive.ID {
			t.Errorf("ListOrphanedApps included app %s on active owner %s (should be filtered)", onActive.ID, a.NodeID)
		}
		if !want[a.ID] {
			t.Errorf("ListOrphanedApps returned unexpected app %s", a.ID)
		}
		delete(want, a.ID)
	}
	if len(want) > 0 {
		t.Errorf("ListOrphanedApps missing orphan apps: %v", want)
	}

	// Empty set when maxPerTick < 1.
	if got, err := m.ListOrphanedApps(ctx, -1, 0); err != nil || len(got) != 0 {
		t.Errorf("ListOrphanedApps(max=0) = %+v, %v; want empty", got, err)
	}

	// Cooldown filter: stamp reassigned_at to now() on the
	// orphan; cooldownSeconds=60 suppresses it.
	if err := m.ReassignAppOwner(ctx, onInactive.ID, onInactive.NodeID, "default-local"); err != nil {
		t.Fatalf("ReassignAppOwner setup: %v", err)
	}
	// The reassigned app is now on default-local — it's no
	// longer an orphan (its new owner is active). Re-stamp
	// it back on the inactive peer to test the cooldown
	// branch.
	peerID := onInactive.NodeID // peer is still inactive
	if err := m.reassignAppOwnerForTest(ctx, onInactive.ID, peerID); err != nil {
		t.Fatalf("re-stamp app back on inactive peer: %v", err)
	}

	if got, err := m.ListOrphanedApps(ctx, 60, 50); err != nil || len(got) != 0 {
		t.Errorf("ListOrphanedApps(cooldown=60) = %+v, %v; want empty (cooldown should suppress)", got, err)
	}

	// Per-tick cap: 1 cap returns exactly 1 app.
	if got, err := m.ListOrphanedApps(ctx, -1, 1); err != nil || len(got) != 1 {
		t.Errorf("ListOrphanedApps(cap=1) = %d, %v; want exactly 1", len(got), err)
	}
}

// TestMemStore_ReassignAppOwner covers the conditional
// UPDATE contract. Three branches: happy reassign, lost
// race (already reassigned by a peer), and missing row.
func TestMemStore_ReassignAppOwner(t *testing.T) {
	m, ctx, _, onInactive, _ := memRebalanceFixture(t)

	// Happy path: peer wins, app moves to default-local.
	if err := m.ReassignAppOwner(ctx, onInactive.ID, onInactive.NodeID, "default-local"); err != nil {
		t.Fatalf("ReassignAppOwner happy: %v", err)
	}
	got, err := m.AppByID(ctx, onInactive.ID)
	if err != nil {
		t.Fatalf("AppByID post-reassign: %v", err)
	}
	if got.NodeID != "default-local" {
		t.Errorf("post-reassign NodeID = %q, want %q", got.NodeID, "default-local")
	}
	if got.ReassignedAt == nil {
		t.Errorf("post-reassign ReassignedAt = nil, want a recent timestamp")
	} else if time.Since(*got.ReassignedAt) > time.Second {
		t.Errorf("post-reassign ReassignedAt = %v, want within the last second", got.ReassignedAt)
	}

	// Lost race: a second peer tries to re-stamp from the
	// old owner; observes ErrConflict.
	err = m.ReassignAppOwner(ctx, onInactive.ID, onInactive.NodeID, "another-peer")
	if !errors.Is(err, ErrConflict) {
		t.Errorf("ReassignAppOwner race: %v; want ErrConflict", err)
	}

	// Missing row.
	err = m.ReassignAppOwner(ctx, "missing-"+uuid.NewString(), "any", "default-local")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("ReassignAppOwner missing: %v; want ErrNotFound", err)
	}

	// Empty arg rejected with a generic error (not a typed sentinel).
	for _, tt := range []struct {
		name            string
		appID, from, to string
	}{
		{"empty appID", "", "any", "default-local"},
		{"empty from", onInactive.ID, "", "default-local"},
		{"empty to", onInactive.ID, "any", ""},
	} {
		if err := m.ReassignAppOwner(ctx, tt.appID, tt.from, tt.to); err == nil {
			t.Errorf("ReassignAppOwner(%s): nil error; want rejection", tt.name)
		}
	}

	// Status filter: the soft-deleted app row's owner is
	// the inactive peer; reassigning it must return
	// ErrConflict (status = AppDeleted is excluded).
	deletedID := "" // not exposed from fixture; query store
	deletedIDFound := false
	for _, a := range m.apps {
		if a.Status == AppDeleted {
			deletedID = a.ID
			deletedIDFound = true
		}
	}
	if !deletedIDFound {
		t.Fatal("soft-deleted app missing from fixture")
	}
	err = m.ReassignAppOwner(ctx, deletedID, onInactive.NodeID, "default-local")
	if !errors.Is(err, ErrConflict) {
		t.Errorf("ReassignAppOwner on soft-deleted: %v; want ErrConflict (status filter)", err)
	}
}

// reassignAppOwnerForTest is a test-only helper that
// re-stamps an app on a target node_id and sets
// ReassignedAt to now(). The exported ReassignAppOwner
// refuses to overwrite a recently-reassigned row; this
// helper bypasses that gate by writing under the test-only
// MemStore mutex.
//
// Kept private + named with the "ForTest" suffix so a
// future contributor doesn't reach for it from production
// code; the only call site is the cooldown-filter branch
// of TestMemStore_ListOrphanedApps.
func (m *MemStore) reassignAppOwnerForTest(_ context.Context, appID, nodeID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.apps[appID]
	if !ok {
		return ErrNotFound
	}
	now := time.Now()
	a.NodeID = nodeID
	a.ReassignedAt = &now
	m.apps[appID] = a
	return nil
}
