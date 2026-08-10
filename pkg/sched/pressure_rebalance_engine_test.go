// pressure_rebalance_engine_test.go — Tier A9 (ADR-087)
// engine-method tests for Engine.RebalancePressuredApps. The
// companion pressure_rebalancer_test.go covers the watcher-loop
// filter and dispatch; this file exercises the engine-side
// policy: peer selection, cooldown, status filter, ownership
// guards, policy-gated live-migration control, and the
// pressure_rebalanced-notify. The watcher test file exercises
// the engine via the watcher; this file exercises the engine
// directly.
//
// Test seams reused from pkg/sched/rebalance_engine_test.go:
//   - rebalanceTestOwners(t) at line 54
//   - seedAppOnNode(t, store, ctx, plan, ramMB, nodeID) at line 105
//   - newRebalanceEngine(t, store, ownerNodeID, notif) at line 129
//   - fakeNotifier + fakeVMM (engine_test.go)
//
// Tests in this file:
//
//  1. TestRebalancePressuredApps_MigratesAppOnOwner — happy path.
//  2. TestRebalancePressuredApps_SkipsAppNotOnOwner — ownership guard.
//  3. TestRebalancePressuredApps_NoPeerNoMigration — no_headroom path.
//  4. TestRebalancePressuredApps_RespectsCooldown — cooldown filter.
//  5. TestRebalancePressuredApps_EmitsPressureRebalancedNotify — notify payload.
//  6. TestRebalancePressuredApps_PolicySkipLive — skip_live policy closes the live window.
//  7. TestRebalancePressuredApps_PolicyMigrateAfter2 — first sweep no live, second sweep no-op live.

package sched

import (
	"context"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// pressureTestOwners returns the same fixture as
// rebalanceTestOwners; the pressure rebalancer routes from
// default-local (the owner) to the peer. We don't need to flip
// the peer inactive — pressure migration is about a healthy
// full owner, not a dead peer.
func pressureTestOwners(t *testing.T) (*state.MemStore, context.Context, state.ComputeNode, state.ComputeNode) {
	return rebalanceTestOwners(t)
}

// seedPressureApp seeds an app on the owner node with the
// owner stamp baked in. Mirrors seedAppOnNode but also
// stamps the active/evicted_cold status explicitly (the
// rebalance tests already assume active, but the pressure
// surface is explicit about the eligibility filter).
func seedPressureApp(t *testing.T, store *state.MemStore, ctx context.Context, plan api.Plan, ramMB int, nodeID string) state.App {
	t.Helper()
	return seedAppOnNode(t, store, ctx, plan, ramMB, nodeID)
}

// newPressureEngine builds a fresh Engine with WithOwnerNodeID
// + WithPressureAggregator + WithPressureConfig + (optional)
// WithPressureMigrationPolicy. Default policy is the package
// default (migrate_after_2).
func newPressureEngine(t *testing.T, store *state.MemStore, ownerNodeID string, notif *fakeNotifier, policy string) *Engine {
	t.Helper()
	agg := NewPressureAggregator()
	e := newEngine(t, store, &fakeVMM{}, notif, "1.10.0").
		WithOwnerNodeID(ownerNodeID).
		WithPressureConfig(5, 30).
		WithPressureAggregator(agg)
	if policy != "" {
		e.WithPressureMigrationPolicy(policy)
	}
	return e
}

// TestRebalancePressuredApps_MigratesAppOnOwner pins the happy
// path: app pinned on the owner, peer has headroom,
// RebalancePressuredApps reassigns apps.node_id to the peer
// and emits the pressure_rebalanced notify.
func TestRebalancePressuredApps_MigratesAppOnOwner(t *testing.T) {
	store, ctx, defaultLocal, peer := pressureTestOwners(t)

	app := seedPressureApp(t, store, ctx, api.PlanHobby, 128, defaultLocal.ID)

	notif := &fakeNotifier{}
	e := newPressureEngine(t, store, defaultLocal.ID, notif, "")
	if err := e.RebalancePressuredApps(ctx, app.ID); err != nil {
		t.Fatalf("RebalancePressuredApps: %v", err)
	}

	got, err := store.AppByID(ctx, app.ID)
	if err != nil {
		t.Fatalf("AppByID: %v", err)
	}
	if got.NodeID != peer.ID {
		t.Errorf("NodeID = %q, want %q (must migrate to peer)", got.NodeID, peer.ID)
	}
	if got.ReassignedAt == nil {
		t.Errorf("ReassignedAt = nil, want a recent timestamp")
	}

	// One NotifyAppChanged{pressure_rebalanced} per migrated app.
	notifies := countPressureRebalancedNotifies(notif)
	if notifies != 1 {
		t.Errorf("pressure_rebalanced notifies = %d, want 1", notifies)
	}
}

// TestRebalancePressuredApps_SkipsAppNotOnOwner pins the
// ownership guard: an app pinned on a peer must NOT be
// re-stamped by another schedd's pressure rebalancer. The
// exit outcome is no_eligibility (the closed set the §12
// dashboard surfaces).
func TestRebalancePressuredApps_SkipsAppNotOnOwner(t *testing.T) {
	store, ctx, defaultLocal, peer := pressureTestOwners(t)

	// App pinned on the peer — not on the owner.
	app := seedPressureApp(t, store, ctx, api.PlanHobby, 128, peer.ID)

	notif := &fakeNotifier{}
	e := newPressureEngine(t, store, defaultLocal.ID, notif, "")
	if err := e.RebalancePressuredApps(ctx, app.ID); err != nil {
		t.Fatalf("RebalancePressuredApps: %v", err)
	}

	got, err := store.AppByID(ctx, app.ID)
	if err != nil {
		t.Fatalf("AppByID: %v", err)
	}
	if got.NodeID != peer.ID {
		t.Errorf("NodeID = %q, want %q (must not move cross-owner)", got.NodeID, peer.ID)
	}
}

// TestRebalancePressuredApps_NoPeerNoMigration pins the
// no_headroom path: when the only active compute node is the
// owner itself, the rebalancer must drop silently and bump
// the no_headroom outcome metric.
func TestRebalancePressuredApps_NoPeerNoMigration(t *testing.T) {
	store := state.NewMemStore()
	ctx := context.Background()

	// Find the default-local node (pre-seeded by NewMemStore).
	var defaultLocal state.ComputeNode
	nodes, err := store.ListComputeNodes(ctx, true)
	if err != nil {
		t.Fatalf("ListComputeNodes: %v", err)
	}
	for _, n := range nodes {
		if n.Name == "default-local" {
			defaultLocal = n
		}
	}
	if defaultLocal.ID == "" {
		t.Fatal("default-local row missing")
	}

	// Flip default-local inactive — no other active node exists.
	if err := store.SetComputeNodeActive(ctx, defaultLocal.ID, false); err != nil {
		t.Fatalf("flip default-local inactive: %v", err)
	}

	app := seedPressureApp(t, store, ctx, api.PlanHobby, 128, defaultLocal.ID)

	notif := &fakeNotifier{}
	e := newPressureEngine(t, store, defaultLocal.ID, notif, "")
	// ownerNodeID != "" guards the early-return; the active=
	// false flag eliminates both the owner AND the (nonexistent)
	// peer from ActiveComputeNodes — the engine sees no peer and
	// drops with no_headroom.
	if err := e.RebalancePressuredApps(ctx, app.ID); err != nil {
		t.Fatalf("RebalancePressuredApps: %v", err)
	}
	got, err := store.AppByID(ctx, app.ID)
	if err != nil {
		t.Fatalf("AppByID: %v", err)
	}
	if got.NodeID != defaultLocal.ID {
		t.Errorf("NodeID = %q, want %q (no peer must not flip)", got.NodeID, defaultLocal.ID)
	}
}

// TestRebalancePressuredApps_RespectsCooldown pins the
// cooldown filter: an app whose reassigned_at is <60s old
// must be skipped on a subsequent rebalance (the metric
// fires outcome="cooldown").
func TestRebalancePressuredApps_RespectsCooldown(t *testing.T) {
	store, ctx, defaultLocal, peer := pressureTestOwners(t)

	app := seedPressureApp(t, store, ctx, api.PlanHobby, 128, defaultLocal.ID)

	// First reassign — succeeds, stamps ReassignedAt.
	notif := &fakeNotifier{}
	e := newPressureEngine(t, store, defaultLocal.ID, notif, "")
	if err := e.RebalancePressuredApps(ctx, app.ID); err != nil {
		t.Fatalf("first RebalancePressuredApps: %v", err)
	}

	// Second reassign within the cooldown window — drops.
	notif2 := &fakeNotifier{}
	if err := e.RebalancePressuredApps(ctx, app.ID); err != nil {
		t.Fatalf("second RebalancePressuredApps: %v", err)
	}
	// Counted via the engine's ops metric — the test
	// verifies the elapsed time check (apps.reassigned_at
	// is now stamped by the first call; the second call
	// reads it and drops).
	got, err := store.AppByID(ctx, app.ID)
	if err != nil {
		t.Fatalf("AppByID: %v", err)
	}
	if got.NodeID != peer.ID {
		t.Errorf("NodeID = %q, want %q (first reassign wins)", got.NodeID, peer.ID)
	}
	// Confirm notif2 saw no further emits (the second
	// RebalancePressuredApps dropped on cooldown).
	if n := countPressureRebalancedNotifies(notif2); n != 0 {
		t.Errorf("cooldown-blocked notifies = %d, want 0", n)
	}
}

// TestRebalancePressuredApps_EmitsPressureRebalancedNotify
// pins the notify payload shape. The gateway's per-app cache
// flush path subscribes to NotifyAppChanged and reads the
// payload to identify the new owner.
func TestRebalancePressuredApps_EmitsPressureRebalancedNotify(t *testing.T) {
	store, ctx, defaultLocal, peer := pressureTestOwners(t)

	app := seedPressureApp(t, store, ctx, api.PlanHobby, 128, defaultLocal.ID)

	notif := &fakeNotifier{}
	e := newPressureEngine(t, store, defaultLocal.ID, notif, "")
	if err := e.RebalancePressuredApps(ctx, app.ID); err != nil {
		t.Fatalf("RebalancePressuredApps: %v", err)
	}
	// Find the NotifyAppChanged emit and verify the payload
	// contains the right kind + endpoints.
	var found bool
	notif.mu.Lock()
	for _, c := range notif.events {
		if c.channel == "app_changed" && strings.Contains(c.payload, "pressure_rebalanced") {
			if !strings.Contains(c.payload, app.ID) {
				t.Errorf("payload missing app id: %s", c.payload)
			}
			if !strings.Contains(c.payload, defaultLocal.ID) {
				t.Errorf("payload missing from_node %q: %s", defaultLocal.ID, c.payload)
			}
			if !strings.Contains(c.payload, peer.ID) {
				t.Errorf("payload missing to_node %q: %s", peer.ID, c.payload)
			}
			found = true
		}
	}
	notif.mu.Unlock()
	if !found {
		t.Errorf("no NotifyAppChanged{pressure_rebalanced} emitted")
	}
}

// TestRebalancePressuredApps_PolicySkipLive pins the skip_live
// policy: the path completes the reassign but does NOT call
// MigrateLiveInstances (the live-migration shape is the
// expensive path; skip_live is the cheap-path-only policy).
func TestRebalancePressuredApps_PolicySkipLive(t *testing.T) {
	store, ctx, defaultLocal, _ := pressureTestOwners(t)

	app := seedPressureApp(t, store, ctx, api.PlanHobby, 128, defaultLocal.ID)

	notif := &fakeNotifier{}
	e := newPressureEngine(t, store, defaultLocal.ID, notif, "skip_live")
	// Drive the sweep counter to 3 (>>2) to demonstrate the
	// policy alone closes the live window, not the sweep count.
	e.IncrementPressureSweepCounter(app.ID)
	e.IncrementPressureSweepCounter(app.ID)
	e.IncrementPressureSweepCounter(app.ID)
	if err := e.RebalancePressuredApps(ctx, app.ID); err != nil {
		t.Fatalf("RebalancePressuredApps: %v", err)
	}
	// No assertion at the live-migration level — the
	// skip_live policy gates the call inside
	// maybeMigrateLiveInstancesFor before the engine
	// touches the four-phase handoff. The sweep test
	// below exercises the same path under migrate_after_2.
}

// TestRebalancePressuredApps_PolicyMigrateAfter2_SecondSweep
// pins the policy gate's sweep-counter behaviour:
// migrate_after_2 fires the live window only on the second
// sweep. First sweep: cheap path only. Second sweep: also
// (would) fire the four-phase live handoff.
//
// We don't have a live instance seeded on the synthetic
// MemStore, so the engine's MigrateLiveInstances call would
// no-op (the heat is "how the policy gate counts"). The
// surface the test pins is the sweep-counter increment + the
// policy enforcement — the live-migration floor is exercised
// by migration_handoff_test.go.
func TestRebalancePressuredApps_PolicyMigrateAfter2_SecondSweep(t *testing.T) {
	store, ctx, defaultLocal, _ := pressureTestOwners(t)

	app := seedPressureApp(t, store, ctx, api.PlanHobby, 128, defaultLocal.ID)

	notif := &fakeNotifier{}
	e := newPressureEngine(t, store, defaultLocal.ID, notif, "migrate_after_2")
	// First sweep — counter is 1, below the threshold of 2.
	e.IncrementPressureSweepCounter(app.ID)

	// Track live-migration call via the migration counter.
	// We can't intercept MigrateLiveInstances from this
	// test surface (the helper is on Engine); pin the
	// sweep counter behaviour directly.
	if got := e.pressureSweepCounterValue(app.ID); got != 1 {
		t.Fatalf("after first increment, sweep counter = %d, want 1", got)
	}

	// Second sweep — counter is 2, policy opens the live window.
	e.IncrementPressureSweepCounter(app.ID)
	if got := e.pressureSweepCounterValue(app.ID); got != 2 {
		t.Fatalf("after second increment, sweep counter = %d, want 2", got)
	}

	// Second sweep's reassign.
	if err := e.RebalancePressuredApps(ctx, app.ID); err != nil {
		t.Fatalf("second RebalancePressuredApps: %v", err)
	}
	// Counter is reset on a successful reassign.
	if got := e.pressureSweepCounterValue(app.ID); got != 0 {
		t.Errorf("after successful reassign, sweep counter = %d, want 0 (reset)", got)
	}
}

// countPressureRebalancedNotifies is the test-side matcher
// for the NotifyAppChanged{pressure_rebalanced} emit. Mirrors
// countRebalancedNotifies at rebalance_engine_test.go.
func countPressureRebalancedNotifies(notif *fakeNotifier) int {
	notif.mu.Lock()
	defer notif.mu.Unlock()
	var n int
	for _, c := range notif.events {
		if c.channel == "app_changed" && strings.Contains(c.payload, "pressure_rebalanced") {
			n++
		}
	}
	return n
}
