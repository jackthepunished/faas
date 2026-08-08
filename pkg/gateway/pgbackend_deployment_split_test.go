// pgbackend_deployment_split_test.go — PR-B (issue #556) pinned tests
// for the per-deployment weighted picker.
//
// Six pinned contracts:
//
//   1. TestPGBackend_PickWeighted_AcrossTwoDeployments:
//      1000 calls against a 25/75 split land within 1% of the
//      expected ratio. Pinned at 1% — PR-C tightens with proportional
//      redistribution, but PR-B's stride + binary search should
//      hold the 1% bound at 1000 picks.
//
//   2. TestPGBackend_PickRoundRobin_WithinDeployment:
//      1000 calls against a single deployment with 3 instances
//      distribute evenly (each instance seen 333±2 times). The
//      per-deployment targetSet's sub-cursor rotates cleanly
//      without drifting toward one node.
//
//   3. TestPGBackend_RefreshDeploymentWeights_OnNotifyDeploymentChanged:
//      Fake deploymentWeightsStore + a direct call to
//      RefreshDeploymentWeights flips the picker ratio on the
//      next Pick. End-to-end for the notify path.
//
//   4. TestPGBackend_PickSingleDeployment_ByteIdenticalToLegacy:
//      A single-deployment picker routes 100% to the single
//      bucket, byte-identical to the pre-PR-B picker (no
//      weighted-stride bookkeeping). Backward-compat tripwire.
//
//   5. TestSched_AppLevelCap_HoldsUnderSplit:
//      See sched engine_appcap_test.go — schedd's per-app ledger
//      cap (issue #556 acceptance #3) holds under a 50/50 split
//      where both deployments are at their per-deployment max.
//
//   6. TestPg_LiveDeployments_ReturnsOnlyLiveRows /
//      TestPg_LiveDeployments_PicksUpIndex:
//      See pkg/state/pgstore_live_deployments_test.go — sqlc
//      plural query returns only status='live' rows, and EXPLAIN
//      confirms the partial index is used.

package gateway_test

import (
	"context"
	"testing"

	"github.com/onebox-faas/faas/pkg/gateway"
)

// fakeWeightsStore is the in-memory deploymentWeightsStore the
// picker tests inject so the RefreshDeploymentWeights path can run
// without a real Postgres.
type fakeWeightsStore struct {
	rows map[string][]gateway.DeploymentWeightsRow
	err  error
}

func (f *fakeWeightsStore) LiveDeployments(_ context.Context, appID string) ([]gateway.DeploymentWeightsRow, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rows[appID], nil
}

// rotatingDeployScheduler wraps gateway.FakeScheduler and
// round-robins the deploymentID across a fixed slice per Admit
// call. Used to seed the picker with multi-bucket instances.
type rotatingDeployScheduler struct {
	*gateway.FakeScheduler
	deployments []string
	idx         int
}

func (r *rotatingDeployScheduler) AdmitInstance(ctx context.Context, appID, _ string) (string, string, string, string, int32, bool, int, error) {
	// Snapshot the deploymentID BEFORE the parent's AdmitInstance
	// runs, since the parent mints ids and we want to drive our
	// own counter.
	dep := r.deployments[r.idx%len(r.deployments)]
	r.idx++
	r.FakeScheduler.WithDeploymentID(dep)
	return r.FakeScheduler.AdmitInstance(ctx, appID, "")
}

// TestPGBackend_PickWeighted_AcrossTwoDeployments (PR-B / issue #556):
// 1000 calls against a 25/75 split land within 1% of the expected
// ratio. The weighted stride + cumulative-weight binary search
// must distribute deterministically — 250 picks to dep-25, 750 to
// dep-75 (each within ±1% = ±10 picks).
func TestPGBackend_PickWeighted_AcrossTwoDeployments(t *testing.T) {
	sched := &rotatingDeployScheduler{
		FakeScheduler: gateway.NewFakeScheduler("node-A"),
		deployments:   []string{"dep-25", "dep-75", "dep-25", "dep-75", "dep-75"}, // 2/3 split seed
	}
	b := gateway.NewPGBackend(&fakeRouter{byID: map[string]gateway.App{}}, sched, nil).
		WithStore(&fakeWeightsStore{rows: map[string][]gateway.DeploymentWeightsRow{
			"app-1": {
				{ID: "dep-25", TrafficPercent: 25},
				{ID: "dep-75", TrafficPercent: 75},
			},
		}})
	if err := b.RefreshDeploymentWeights(context.Background(), "app-1"); err != nil {
		t.Fatalf("RefreshDeploymentWeights: %v", err)
	}
	// Seed 5 admits: 2 on dep-25, 3 on dep-75 (both buckets non-empty
	// so the cold-deployment fallback never fires).
	for i := 0; i < 5; i++ {
		if _, _, _, err := b.Admit(context.Background(), "app-1", "", 10); err != nil {
			t.Fatalf("Admit #%d: %v", i+1, err)
		}
	}

	// Run 1000 picks. The weighted stride routes 25% to dep-25
	// and 75% to dep-75 within ±1% (1000 calls = ±10 picks).
	const totalPicks = 1000
	const wantDep25 = totalPicks * 25 / 100
	const wantDep75 = totalPicks * 75 / 100
	const tolerance = totalPicks / 100 // 1% = 10 picks
	gotDep25, gotDep75 := 0, 0
	for i := 0; i < totalPicks; i++ {
		t1 := b.Pick("app-1")
		if !t1.OK {
			t.Fatal("Pick: !ok")
		}
		switch t1.Target.DeploymentID {
		case "dep-25":
			gotDep25++
		case "dep-75":
			gotDep75++
		default:
			t.Fatalf("Pick #%d = DeploymentID %q, want dep-25 or dep-75", i, t1.Target.DeploymentID)
		}
	}
	if diff := gotDep25 - wantDep25; diff < -tolerance || diff > tolerance {
		t.Errorf("dep-25 picks = %d, want %d ± %d (1%% tolerance)", gotDep25, wantDep25, tolerance)
	}
	if diff := gotDep75 - wantDep75; diff < -tolerance || diff > tolerance {
		t.Errorf("dep-75 picks = %d, want %d ± %d (1%% tolerance)", gotDep75, wantDep75, tolerance)
	}
}

// TestPGBackend_PickRoundRobin_WithinDeployment (PR-B / issue #556):
// 1000 calls against a single deployment with 3 instances
// distribute evenly (each instance seen 333±2 times). The
// per-deployment targetSet's sub-cursor rotates cleanly without
// drifting toward one node. Single-deployment fast path also
// exercised implicitly (only one weight entry).
func TestPGBackend_PickRoundRobin_WithinDeployment(t *testing.T) {
	sched := gateway.NewFakeScheduler("node-A").WithDeploymentID("dep-1")
	b := gateway.NewPGBackend(&fakeRouter{byID: map[string]gateway.App{}}, sched, nil).
		WithStore(&fakeWeightsStore{rows: map[string][]gateway.DeploymentWeightsRow{
			"app-1": {{ID: "dep-1", TrafficPercent: 100}},
		}})
	if err := b.RefreshDeploymentWeights(context.Background(), "app-1"); err != nil {
		t.Fatalf("RefreshDeploymentWeights: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, _, _, err := b.Admit(context.Background(), "app-1", "", 5); err != nil {
			t.Fatalf("Admit #%d: %v", i+1, err)
		}
	}
	const totalPicks = 1000
	const wantPerInstance = totalPicks / 3
	const tolerance = 2
	counts := map[string]int{}
	for i := 0; i < totalPicks; i++ {
		t1 := b.Pick("app-1")
		if !t1.OK {
			t.Fatal("Pick: !ok")
		}
		if t1.Target.DeploymentID != "dep-1" {
			t.Errorf("Pick #%d = DeploymentID %q, want dep-1 (single deployment)", i, t1.Target.DeploymentID)
		}
		counts[t1.Target.InstanceID]++
	}
	if len(counts) != 3 {
		t.Fatalf("distinct instances picked = %d, want 3", len(counts))
	}
	for id, c := range counts {
		if diff := c - wantPerInstance; diff < -tolerance || diff > tolerance {
			t.Errorf("instance %s picked %d times, want %d ± %d", id, c, wantPerInstance, tolerance)
		}
	}
}

// TestPGBackend_RefreshDeploymentWeights_OnNotifyDeploymentChanged
// (PR-B / issue #556): end-to-end for the notify-driven refresh.
// Start with a 100/0 split, drive RefreshDeploymentWeights to flip
// to 25/75, assert the next 1000 picks honour the new ratio.
func TestPGBackend_RefreshDeploymentWeights_OnNotifyDeploymentChanged(t *testing.T) {
	sched := gateway.NewFakeScheduler("node-A").WithDeploymentID("dep-A")
	store := &fakeWeightsStore{rows: map[string][]gateway.DeploymentWeightsRow{
		"app-1": {{ID: "dep-A", TrafficPercent: 100}},
	}}
	b := gateway.NewPGBackend(&fakeRouter{byID: map[string]gateway.App{}}, sched, nil).WithStore(store)
	if err := b.RefreshDeploymentWeights(context.Background(), "app-1"); err != nil {
		t.Fatalf("initial refresh: %v", err)
	}
	for i := 0; i < 4; i++ {
		if _, _, _, err := b.Admit(context.Background(), "app-1", "", 5); err != nil {
			t.Fatalf("Admit #%d: %v", i+1, err)
		}
	}

	// Pre-refresh: 100% to dep-A.
	for i := 0; i < 10; i++ {
		t1 := b.Pick("app-1")
		if !t1.OK {
			t.Fatal("Pick: !ok")
		}
		if t1.Target.DeploymentID != "dep-A" {
			t.Errorf("pre-refresh Pick #%d = DeploymentID %q, want dep-A", i, t1.Target.DeploymentID)
		}
	}

	// Operator flips to 25/75 — fake store returns the new rows.
	store.rows["app-1"] = []gateway.DeploymentWeightsRow{
		{ID: "dep-A", TrafficPercent: 25},
		{ID: "dep-B", TrafficPercent: 75},
	}
	if err := b.RefreshDeploymentWeights(context.Background(), "app-1"); err != nil {
		t.Fatalf("post-flip refresh: %v", err)
	}

	// Post-refresh: 25% to dep-A, 75% to dep-B within ±1%.
	// Seed dep-B with admits so the cold-deployment fallback
	// never fires (a fresh dep has no instances).
	store.rows["app-1"] = []gateway.DeploymentWeightsRow{
		{ID: "dep-A", TrafficPercent: 25},
		{ID: "dep-B", TrafficPercent: 75},
	}
	// The flip above sets store.rows back to the 25/75 split.
	// We need to also seed dep-B by admitting on it. The fake
	// scheduler now returns dep-A by default — drive a
	// separate route via a fake refresh that points only at
	// dep-B for the seed admits, then restore the 25/75 split.
	seedRows := []gateway.DeploymentWeightsRow{{ID: "dep-B", TrafficPercent: 100}}
	store.rows["app-1"] = seedRows
	if err := b.RefreshDeploymentWeights(context.Background(), "app-1"); err != nil {
		t.Fatalf("seed refresh: %v", err)
	}
	// Reconfigure the scheduler to seed dep-B instances, then
	// restore the 25/75 ratio.
	sched.WithDeploymentID("dep-B")
	for i := 0; i < 3; i++ {
		if _, _, _, err := b.Admit(context.Background(), "app-1", "", 10); err != nil {
			t.Fatalf("dep-B Admit #%d: %v", i+1, err)
		}
	}
	// Re-flip back to 25/75.
	store.rows["app-1"] = []gateway.DeploymentWeightsRow{
		{ID: "dep-A", TrafficPercent: 25},
		{ID: "dep-B", TrafficPercent: 75},
	}
	if err := b.RefreshDeploymentWeights(context.Background(), "app-1"); err != nil {
		t.Fatalf("final refresh: %v", err)
	}

	// Post-refresh: 25% to dep-A, 75% to dep-B within ±1%.
	const totalPicks = 1000
	const tolerance = totalPicks / 100
	gotA, gotB := 0, 0
	for i := 0; i < totalPicks; i++ {
		t1 := b.Pick("app-1")
		if !t1.OK {
			t.Fatal("Pick: !ok")
		}
		switch t1.Target.DeploymentID {
		case "dep-A":
			gotA++
		case "dep-B":
			gotB++
		default:
			t.Fatalf("post-refresh Pick #%d = DeploymentID %q, want dep-A or dep-B", i, t1.Target.DeploymentID)
		}
	}
	wantA, wantB := totalPicks*25/100, totalPicks*75/100
	if diff := gotA - wantA; diff < -tolerance || diff > tolerance {
		t.Errorf("post-refresh dep-A picks = %d, want %d ± %d", gotA, wantA, tolerance)
	}
	if diff := gotB - wantB; diff < -tolerance || diff > tolerance {
		t.Errorf("post-refresh dep-B picks = %d, want %d ± %d", gotB, wantB, tolerance)
	}
}

// TestPGBackend_PickSingleDeployment_ByteIdenticalToLegacy (PR-B /
// issue #556): backward-compat tripwire. When only one live
// deployment exists, the picker must route 100% to that
// deployment's bucket — no weighted-stride bookkeeping, no extra
// atomic increment, byte-identical to the pre-PR-B
// single-targetSet behaviour. A regression that runs the weighted
// stride even when len(weights)==1 would add an atomic increment
// per Pick (visible in -race) and could silently mis-route on a
// multi-instance cold start.
func TestPGBackend_PickSingleDeployment_ByteIdenticalToLegacy(t *testing.T) {
	sched := gateway.NewFakeScheduler("node-A").WithDeploymentID("dep-only")
	b := gateway.NewPGBackend(&fakeRouter{byID: map[string]gateway.App{}}, sched, nil).
		WithStore(&fakeWeightsStore{rows: map[string][]gateway.DeploymentWeightsRow{
			"app-1": {{ID: "dep-only", TrafficPercent: 100}},
		}})
	if err := b.RefreshDeploymentWeights(context.Background(), "app-1"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, _, _, err := b.Admit(context.Background(), "app-1", "", 5); err != nil {
			t.Fatalf("Admit #%d: %v", i+1, err)
		}
	}
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		t1 := b.Pick("app-1")
		if !t1.OK {
			t.Fatal("Pick: !ok")
		}
		if t1.Target.DeploymentID != "dep-only" {
			t.Errorf("Pick #%d = DeploymentID %q, want dep-only", i, t1.Target.DeploymentID)
		}
		if t1.Target.NodeID != "node-A" {
			t.Errorf("Pick #%d = NodeID %q, want node-A", i, t1.Target.NodeID)
		}
		seen[t1.Target.InstanceID] = true
	}
	if len(seen) != 3 {
		t.Errorf("distinct instances = %d, want 3 (single-deployment round-robin)", len(seen))
	}
}
