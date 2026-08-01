package instancestats

import (
	"testing"
	"time"
)

func TestReader_Empty(t *testing.T) {
	r := NewReader()
	if got := r.SnapshotAll(); got != nil {
		t.Errorf("SnapshotAll on empty Reader = %v, want nil", got)
	}
	if got := r.SnapshotForApp("app1"); got != nil {
		t.Errorf("SnapshotForApp on empty Reader = %v, want nil", got)
	}
	if _, ok := r.SnapshotForInstance("i-1"); ok {
		t.Error("SnapshotForInstance on empty Reader returned ok=true")
	}
}

func TestReader_ReplaceSnapshotAllReturnsRows(t *testing.T) {
	r := NewReader()
	now := time.Now()
	r.Replace([]InstanceStat{
		{AppID: "app1", InstanceID: "i-2", NodeID: "n1", SampledAt: now},
		{AppID: "app1", InstanceID: "i-1", NodeID: "n1", SampledAt: now},
		{AppID: "app2", InstanceID: "i-3", NodeID: "n1", SampledAt: now},
	})
	got := r.SnapshotAll()
	if len(got) != 3 {
		t.Fatalf("SnapshotAll len = %d, want 3", len(got))
	}
	// Deterministic (app, instance) order. Expected: app1/i-1,
	// app1/i-2, app2/i-3.
	if got[0].InstanceID != "i-1" || got[1].InstanceID != "i-2" || got[2].InstanceID != "i-3" {
		t.Errorf("SnapshotAll order = %s, %s, %s; want i-1, i-2, i-3", got[0].InstanceID, got[1].InstanceID, got[2].InstanceID)
	}
}

func TestReader_ReplaceSnapshotForAppFilters(t *testing.T) {
	r := NewReader()
	now := time.Now()
	r.Replace([]InstanceStat{
		{AppID: "app1", InstanceID: "i-1", NodeID: "n1", SampledAt: now},
		{AppID: "app2", InstanceID: "i-2", NodeID: "n1", SampledAt: now},
		{AppID: "app1", InstanceID: "i-3", NodeID: "n1", SampledAt: now},
	})
	got := r.SnapshotForApp("app1")
	if len(got) != 2 {
		t.Fatalf("SnapshotForApp app1 len = %d, want 2", len(got))
	}
	ids := []string{got[0].InstanceID, got[1].InstanceID}
	want := []string{"i-1", "i-3"}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("SnapshotForApp app1[%d] = %q, want %q", i, ids[i], want[i])
		}
	}
}

func TestReader_ReplaceSnapshotForInstance(t *testing.T) {
	r := NewReader()
	now := time.Now()
	r.Replace([]InstanceStat{
		{AppID: "app1", InstanceID: "i-1", NodeID: "n1", SampledAt: now, CPUPct: 50, RSSMB: 256},
	})
	got, ok := r.SnapshotForInstance("i-1")
	if !ok {
		t.Fatal("SnapshotForInstance i-1: not found")
	}
	if got.CPUPct != 50 || got.RSSMB != 256 {
		t.Errorf("SnapshotForInstance i-1 = %+v, want CPUPct=50 RSSMB=256", got)
	}
	if _, ok := r.SnapshotForInstance("nope"); ok {
		t.Error("SnapshotForInstance nope: found, want not found")
	}
}

func TestReader_DeterministicOrdering(t *testing.T) {
	r := NewReader()
	now := time.Now()
	// Insert in deliberately scrambled order. Replace must
	// emit rows in (AppID, InstanceID) order.
	r.Replace([]InstanceStat{
		{AppID: "zeta", InstanceID: "i-1", SampledAt: now},
		{AppID: "alpha", InstanceID: "i-9", SampledAt: now},
		{AppID: "alpha", InstanceID: "i-1", SampledAt: now},
		{AppID: "alpha", InstanceID: "i-2", SampledAt: now},
		{AppID: "zeta", InstanceID: "i-2", SampledAt: now},
	})
	got := r.SnapshotAll()
	want := []string{"alpha/i-1", "alpha/i-2", "alpha/i-9", "zeta/i-1", "zeta/i-2"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		gotKey := got[i].AppID + "/" + got[i].InstanceID
		if gotKey != want[i] {
			t.Errorf("SnapshotAll[%d] = %s, want %s", i, gotKey, want[i])
		}
	}
}

// TestReader_ReplaceAtomicVisibility is now covered as the
// PropertyReader_NoTornReads deterministic property test in
// reader_property_test.go — keeping a single property test there
// and a stable inventory in this file.

// TestReader_MaxInflightForApp pins the PR-B (issue #462)
// accessor across the three outcomes that matter to PR-C's
// scale-up trigger:
//
//   - app not in snapshot  → (0, false) — "no signal", caller
//     falls back to "do not scale" semantics.
//   - app present, all InflightRequests == 0 → (0, true) —
//     "app has live instances but they are idle", caller
//     treats as a valid zero reading.
//   - app present, max = 5 → (5, true) — load-bearing pin
//     the trigger compares against target.concurrent_requests.
func TestReader_MaxInflightForApp(t *testing.T) {
	now := time.Now()

	t.Run("AppNotInSnapshot", func(t *testing.T) {
		r := NewReader()
		r.Replace([]InstanceStat{
			{AppID: "app-other", InstanceID: "i-1", SampledAt: now, InflightRequests: 9},
		})
		got, ok := r.MaxInflightForApp("app-missing")
		if ok || got != 0 {
			t.Errorf("MaxInflightForApp(missing) = (%d, %v), want (0, false)", got, ok)
		}
	})

	t.Run("AppPresentAllIdle", func(t *testing.T) {
		r := NewReader()
		r.Replace([]InstanceStat{
			{AppID: "app1", InstanceID: "i-1", SampledAt: now, InflightRequests: 0},
			{AppID: "app1", InstanceID: "i-2", SampledAt: now, InflightRequests: 0},
		})
		got, ok := r.MaxInflightForApp("app1")
		if !ok {
			t.Errorf("MaxInflightForApp(app1) ok=false, want true (app has live instances)")
		}
		if got != 0 {
			t.Errorf("MaxInflightForApp(app1) = %d, want 0 (all instances idle)", got)
		}
	})

	t.Run("AppPresentReturnsMax", func(t *testing.T) {
		r := NewReader()
		r.Replace([]InstanceStat{
			{AppID: "app1", InstanceID: "i-1", SampledAt: now, InflightRequests: 2},
			{AppID: "app1", InstanceID: "i-2", SampledAt: now, InflightRequests: 5},
			{AppID: "app1", InstanceID: "i-3", SampledAt: now, InflightRequests: 1},
			// Different app must NOT contribute to the max.
			{AppID: "app-other", InstanceID: "i-9", SampledAt: now, InflightRequests: 99},
		})
		got, ok := r.MaxInflightForApp("app1")
		if !ok {
			t.Fatalf("MaxInflightForApp(app1) ok=false, want true")
		}
		if got != 5 {
			t.Errorf("MaxInflightForApp(app1) = %d, want 5 (max across i-1..i-3, ignoring app-other)", got)
		}
	})
}
