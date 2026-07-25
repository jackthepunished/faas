package sched

import (
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

func TestEffectiveIdleTimeout(t *testing.T) {
	tests := []struct {
		plan       api.Plan
		configured int
		want       int
	}{
		{api.PlanFree, 0, 30},    // default
		{api.PlanPro, 0, 300},    // default
		{api.PlanPro, 120, 120},  // in-bounds override
		{api.PlanPro, 5, 10},     // below floor → 10
		{api.PlanPro, 9999, 600}, // above ceiling (300×2) → 600
		{api.PlanFree, 100, 60},  // Free ceiling = 30×2
	}
	for _, tt := range tests {
		if got := EffectiveIdleTimeoutS(tt.plan, tt.configured); got != tt.want {
			t.Errorf("EffectiveIdleTimeoutS(%s, %d) = %d, want %d", tt.plan, tt.configured, got, tt.want)
		}
	}
}

func TestReapIdle(t *testing.T) {
	now := time.Now()
	instances := []InstanceInfo{
		// Pro default 300s; idle 400s → reap.
		{Instance: "idle", Plan: api.PlanPro, State: state.StateRunning, LastRequest: now.Add(-400 * time.Second)},
		// Pro; idle 100s → keep.
		{Instance: "busy", Plan: api.PlanPro, State: state.StateRunning, LastRequest: now.Add(-100 * time.Second)},
		// Idle but not running → not reapable.
		{Instance: "waking", Plan: api.PlanPro, State: state.StateWaking, LastRequest: now.Add(-999 * time.Second)},
		// Free 30s; idle 45s → reap.
		{Instance: "free-idle", Plan: api.PlanFree, State: state.StateRunning, LastRequest: now.Add(-45 * time.Second)},
	}
	got := ReapIdle(now, instances)
	if !equalSet(got, []string{"idle", "free-idle"}) {
		t.Errorf("ReapIdle = %v, want [idle free-idle]", got)
	}
}

// TestReapIdleSkipsInstanceWithOpenConns pins spec §17 G7: an instance
// with OpenConns > 0 is considered active regardless of LastRequest
// staleness. Without this, a parked app mid-WebSocket would be reaped
// on the next idle tick (60 s on Hobby) and the connection would
// close. The five cases fence the behaviour on every side:
//
//   - open=0 + stale LastRequest → reaped (regression: old rule fires)
//   - open>0 + stale LastRequest → NOT reaped (the G7 fix)
//   - open>0 + recent LastRequest → not reaped (no double-counting)
//   - open>0 + zero LastRequest (never-seen) → not reaped
//   - open=0 + recent LastRequest → not reaped (control)
func TestReapIdleSkipsInstanceWithOpenConns(t *testing.T) {
	now := time.Now()
	instances := []InstanceInfo{
		// G7 fix.
		{Instance: "open-stale", Plan: api.PlanPro, State: state.StateRunning,
			LastRequest: now.Add(-time.Hour), OpenConns: 3},
		// No regression: still reaped when no flow + stale.
		{Instance: "idle", Plan: api.PlanPro, State: state.StateRunning,
			LastRequest: now.Add(-time.Hour)},
		// Active + open flows: not reaped.
		{Instance: "open-fresh", Plan: api.PlanPro, State: state.StateRunning,
			LastRequest: now.Add(-time.Second), OpenConns: 1},
		// Never-seen + open: not reaped (TCP active w/ no HTTP).
		{Instance: "open-zero-last", Plan: api.PlanHobby, State: state.StateRunning,
			OpenConns: 2},
		// Active + no flow: not reaped.
		{Instance: "fresh", Plan: api.PlanPro, State: state.StateRunning,
			LastRequest: now.Add(-time.Second)},
	}
	got := ReapIdle(now, instances)
	if !equalSet(got, []string{"idle"}) {
		t.Errorf("ReapIdle = %v, want [idle] only", got)
	}
}

func TestSelectEvictionsBelowThresholdNoop(t *testing.T) {
	got := SelectEvictions(EvictionThresholdMB, time.Now(), []InstanceInfo{
		{Instance: "x", Plan: api.PlanPro, State: state.StateRunning},
	})
	if got != nil {
		t.Errorf("no eviction expected at/below threshold, got %v", got)
	}
}

func TestSelectEvictionsLRUAndScaleLast(t *testing.T) {
	now := time.Now()
	old := now.Add(-time.Hour)
	// Resident well over threshold; each Pro instance is 520 MB (512+8).
	instances := []InstanceInfo{
		{Instance: "scale-oldest", Plan: api.PlanScale, State: state.StateRunning, RAMMB: 1024, LastRequest: old.Add(-time.Hour), Started: old},
		{Instance: "pro-newest", Plan: api.PlanPro, State: state.StateRunning, RAMMB: 512, LastRequest: now.Add(-time.Minute), Started: old},
		{Instance: "pro-oldest", Plan: api.PlanPro, State: state.StateRunning, RAMMB: 512, LastRequest: old, Started: old},
	}
	// Just 1 MB over threshold → evict exactly one; must be the LRU non-Scale.
	got := SelectEvictions(EvictionThresholdMB+1, now, instances)
	if len(got) != 1 || got[0] != "pro-oldest" {
		t.Errorf("expected [pro-oldest] (LRU non-Scale), got %v", got)
	}
}

func TestSelectEvictionsProtectsYoungInstances(t *testing.T) {
	now := time.Now()
	instances := []InstanceInfo{
		// Over threshold but only a 5s-old instance exists → protected.
		{Instance: "fresh", Plan: api.PlanPro, State: state.StateRunning, RAMMB: 512, LastRequest: now.Add(-time.Hour), Started: now.Add(-5 * time.Second)},
	}
	got := SelectEvictions(EvictionThresholdMB+1000, now, instances)
	if len(got) != 0 {
		t.Errorf("instance younger than %s must not be evicted, got %v", MinInstanceAge, got)
	}
}

func TestSelectEvictionsEvictsEnough(t *testing.T) {
	now := time.Now()
	old := now.Add(-time.Hour)
	var instances []InstanceInfo
	for i := 0; i < 10; i++ {
		instances = append(instances, InstanceInfo{
			Instance: string(rune('a' + i)), Plan: api.PlanPro, State: state.StateRunning,
			RAMMB: 512, LastRequest: old.Add(time.Duration(i) * time.Minute), Started: old,
		})
	}
	// 2080 MB over threshold; each frees 520 MB → need 4 evictions.
	got := SelectEvictions(EvictionThresholdMB+2080, now, instances)
	if len(got) != 4 {
		t.Errorf("expected 4 evictions to clear 2080 MB at 520 MB each, got %d (%v)", len(got), got)
	}
}

func equalSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, s := range a {
		m[s]++
	}
	for _, s := range b {
		m[s]--
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return true
}

// TestReapIdleRespectsMinInstancesFloor pins ux_spec §6.5: when an
// app's MinInstances > 0, the reaper must keep at least that many
// RUNNING instances alive regardless of idle timeout. Direction:
// drop the FRESHEST candidates (not the most-idle ones) so the
// freshly-woken instance that just served a user stays resident.
//
// Layout: 4 stale Pro instances of the same app. Floor 0 → all 4
// reaped (matches TestReapIdle behaviour). Floor 2 → 2 reaped
// (the two oldest by LastRequest). Floor 4 → 0 reaped. Floor
// larger than running → 0 reaped (degenerate but bounded).
func TestReapIdleRespectsMinInstancesFloor(t *testing.T) {
	now := time.Now()
	mkApp := func(id string, lastSeen time.Duration) InstanceInfo {
		return InstanceInfo{
			Instance: id, AppID: "app1", Plan: api.PlanPro,
			State: state.StateRunning, LastRequest: now.Add(-lastSeen),
		}
	}
	instances := []InstanceInfo{
		mkApp("oldest", time.Hour), // most idle → reap first
		mkApp("older", 45*time.Minute),
		mkApp("newer", 30*time.Minute),
		mkApp("newest", 15*time.Minute), // freshest → reap last
	}
	for _, in := range instances {
		in.MinInstances = 0 // start with no floor
	}
	// floor 0 → reap all 4.
	got := ReapIdle(now, instances)
	if !equalSet(got, []string{"oldest", "older", "newer", "newest"}) {
		t.Fatalf("floor 0: got %v, want all 4", got)
	}
	// floor 2 → reap 2 oldest.
	for i := range instances {
		instances[i].MinInstances = 2
	}
	got = ReapIdle(now, instances)
	if !equalSet(got, []string{"oldest", "older"}) {
		t.Fatalf("floor 2: got %v, want [oldest older] (drop freshest)", got)
	}
	// floor 4 → reap 0.
	for i := range instances {
		instances[i].MinInstances = 4
	}
	got = ReapIdle(now, instances)
	if len(got) != 0 {
		t.Fatalf("floor 4 (== running): got %v, want empty", got)
	}
	// floor 99 (degenerate; per-row) → reap 0. allowed = running(4) - 99 < 0.
	for i := range instances {
		instances[i].MinInstances = 99
	}
	got = ReapIdle(now, instances)
	if len(got) != 0 {
		t.Fatalf("floor 99 (>running): got %v, want empty (allowed clamps to 0)", got)
	}
}

// TestReapIdleFloorDoesNotCrossApps locks in that the floor is
// per-app: app1's floor 2 must not reduce app2's park count and
// vice versa. Two stale instances of app1, three of app2 — both
// app1 and app2 have floor 1 → app1 reaps 1 (keeps 1), app2
// reaps 2 (keeps 1).
func TestReapIdleFloorDoesNotCrossApps(t *testing.T) {
	now := time.Now()
	mkApp := func(app, id string, lastSeen time.Duration) InstanceInfo {
		return InstanceInfo{
			Instance: id, AppID: app, Plan: api.PlanPro,
			State: state.StateRunning, LastRequest: now.Add(-lastSeen),
		}
	}
	instances := []InstanceInfo{
		mkApp("a1", "a1-old", time.Hour),
		mkApp("a1", "a1-new", 30*time.Minute),
		mkApp("a2", "a2-old", time.Hour),
		mkApp("a2", "a2-mid", 45*time.Minute),
		mkApp("a2", "a2-new", 15*time.Minute),
	}
	for i := range instances {
		instances[i].MinInstances = 1
	}
	got := ReapIdle(now, instances)
	if !equalSet(got, []string{"a1-old", "a2-old", "a2-mid"}) {
		t.Fatalf("got %v, want [a1-old a2-old a2-mid] (1 fresh per app kept)", got)
	}
}

// TestSelectEvictionsIgnoresMinInstances pins R5 from the plan:
// RAM-pressure eviction ignores the floor because the ceiling
// (inv §6.2-2) wins. Floor is budget, ceiling is physics.
func TestSelectEvictionsIgnoresMinInstances(t *testing.T) {
	now := time.Now()
	old := now.Add(-time.Hour)
	// 1 Pro instance, RAMMB 512, floor 1 → still evictable under
	// RAM pressure because SelectEvictions is intentionally
	// floor-blind (spec §6.2-2: ceiling wins).
	instances := []InstanceInfo{
		{Instance: "warm", AppID: "app1", Plan: api.PlanPro,
			State: state.StateRunning, RAMMB: 512,
			LastRequest: old, Started: old, MinInstances: 1},
	}
	got := SelectEvictions(EvictionThresholdMB+1, now, instances)
	if len(got) != 1 || got[0] != "warm" {
		t.Fatalf("RAM-pressure eviction must override the floor; got %v, want [warm]", got)
	}
}

// ---------------------------------------------------------------------
// ReapAggressive (issue #171): aggressive reaper scale-down based on
// the recent-load signal. The pure function takes a precomputed
// `desiredByApp` map and returns instance IDs to park.
//
// These tests are pure: a caller-supplied `now` and a hand-built
// []InstanceInfo drive every branch — no clock, no DB.
// ---------------------------------------------------------------------

// mkAggressive is a test helper: build an InstanceInfo with the
// fields ReapAggressive reads. Started is set to now-1h so the
// MinInstanceAge filter does not incidentally protect a row that
// the test means to be a reap candidate.
func mkAggressive(app, id string, lastSeen time.Duration, open int64, minInst int) InstanceInfo {
	now := time.Now()
	return InstanceInfo{
		Instance: id, AppID: app, Plan: api.PlanPro,
		State: state.StateRunning,
		LastRequest: now.Add(-lastSeen),
		Started: now.Add(-time.Hour),
		OpenConns: open,
		MinInstances: minInst,
	}
}

// TestReapAggressive_ParkToBuffer pins the headline acceptance
// scenario: 5 instances, desired=0, floor=1. The +1 hysteresis
// buffer keeps one warm, so 4 are parked; the freshest survives.
func TestReapAggressive_ParkToBuffer(t *testing.T) {
	now := time.Now()
	instances := []InstanceInfo{
		mkAggressive("app1", "oldest", time.Hour, 0, 1),
		mkAggressive("app1", "older", 45*time.Minute, 0, 1),
		mkAggressive("app1", "newer", 30*time.Minute, 0, 1),
		mkAggressive("app1", "newest", 15*time.Minute, 0, 1),
	}
	got := ReapAggressive(now, instances, map[string]int{"app1": 0})
	// limit = max(1, 0+1) = 1; extra = 4-1 = 3. Wait — 4 candidates, 3
	// extra → park 3, keep the freshest.
	if !equalSet(got, []string{"oldest", "older", "newer"}) {
		t.Fatalf("5-inst / desired=0 / floor=1: got %v, want [oldest older newer]", got)
	}
}

// TestReapAggressive_WithinBuffer: desired=2 means we want 2 warm.
// With +1 buffer, limit=3. running=3, extra=0 → no park.
func TestReapAggressive_WithinBuffer(t *testing.T) {
	now := time.Now()
	instances := []InstanceInfo{
		mkAggressive("app1", "a", time.Hour, 0, 0),
		mkAggressive("app1", "b", 45*time.Minute, 0, 0),
		mkAggressive("app1", "c", 30*time.Minute, 0, 0),
	}
	got := ReapAggressive(now, instances, map[string]int{"app1": 2})
	if len(got) != 0 {
		t.Fatalf("3-inst / desired=2 / within buffer: got %v, want empty", got)
	}
}

// TestReapAggressive_JustAboveBuffer: desired=1, running=3,
// limit=max(0,1+1)=2, extra=1 → park 1 oldest.
func TestReapAggressive_JustAboveBuffer(t *testing.T) {
	now := time.Now()
	instances := []InstanceInfo{
		mkAggressive("app1", "oldest", time.Hour, 0, 0),
		mkAggressive("app1", "mid", 45*time.Minute, 0, 0),
		mkAggressive("app1", "newest", 30*time.Minute, 0, 0),
	}
	got := ReapAggressive(now, instances, map[string]int{"app1": 1})
	if len(got) != 1 || got[0] != "oldest" {
		t.Fatalf("got %v, want [oldest]", got)
	}
}

// TestReapAggressive_OpenConnsProtect: G7 still wins. An instance
// with OpenConns > 0 counts toward running (it's live) but never
// enters the candidate set. Layout: 3 running, of which 1 has
// open flows. running=3, candidates=2, limit=max(0, 0+1)=1,
// extra=3-1=2 → park 2 oldest (BOTH flow-less instances). The
// open-conns one survives alone. The test pins that
// "open-conns protects" — the survivor set MUST include the
// flow-holding instance and the park set MUST NOT.
func TestReapAggressive_OpenConnsProtect(t *testing.T) {
	now := time.Now()
	instances := []InstanceInfo{
		mkAggressive("app1", "oldest", time.Hour, 0, 0),
		mkAggressive("app1", "open", 45*time.Minute, 3, 0),
		mkAggressive("app1", "newest", 30*time.Minute, 0, 0),
	}
	got := ReapAggressive(now, instances, map[string]int{"app1": 0})
	if !equalSet(got, []string{"oldest", "newest"}) {
		t.Fatalf("got %v, want [oldest newest] (open-conns instance is the only survivor)", got)
	}
}

// TestReapAggressive_MinInstanceAgeProtects: a freshly-woken
// instance (Started = now-10s) must never be reaped by the
// aggressive path, even if the buffer says to. This is the same
// rule SelectEvictions enforces.
func TestReapAggressive_MinInstanceAgeProtects(t *testing.T) {
	now := time.Now()
	fresh := InstanceInfo{
		Instance: "fresh", AppID: "app1", Plan: api.PlanPro,
		State: state.StateRunning,
		LastRequest: now.Add(-time.Hour),
		Started: now.Add(-10 * time.Second), // younger than MinInstanceAge (30s)
		MinInstances: 0,
	}
	instances := []InstanceInfo{
		fresh,
		mkAggressive("app1", "old", time.Hour, 0, 0),
	}
	got := ReapAggressive(now, instances, map[string]int{"app1": 0})
	// fresh: not in candidates (too young). old: in candidates.
	// limit=max(0, 0+1)=1; extra=2-1=1; park 1 = "old".
	if !equalSet(got, []string{"old"}) {
		t.Fatalf("got %v, want [old] (young instance protected)", got)
	}
}

// TestReapAggressive_SingleInstanceApp: running=1 cannot exceed
// floor=0 + (desired+1) ≥ 1, so extra is always ≤ 0 → no park.
func TestReapAggressive_SingleInstanceApp(t *testing.T) {
	now := time.Now()
	instances := []InstanceInfo{mkAggressive("app1", "only", time.Hour, 0, 0)}
	got := ReapAggressive(now, instances, map[string]int{"app1": 0})
	if len(got) != 0 {
		t.Fatalf("single instance must not be reaped, got %v", got)
	}
}

// TestReapAggressive_NilDesired_DefersToReapIdle: apps absent
// from desiredByApp are skipped entirely. Hobby/Free apps without
// autoscale configured must not be touched by the aggressive path.
func TestReapAggressive_NilDesired_DefersToReapIdle(t *testing.T) {
	now := time.Now()
	instances := []InstanceInfo{
		mkAggressive("hobby-app", "a", time.Hour, 0, 0),
		mkAggressive("hobby-app", "b", 45*time.Minute, 0, 0),
	}
	// desiredByApp has no entry for hobby-app.
	got := ReapAggressive(now, instances, nil)
	if len(got) != 0 {
		t.Fatalf("apps absent from desiredByApp must not be reaped, got %v", got)
	}
}

// TestReapAggressive_ZeroTarget_ClampsToFloor: desired=0,
// floor=2, running=5. limit=max(2, 0+1)=2, extra=3 → park 3
// oldest.
func TestReapAggressive_ZeroTarget_ClampsToFloor(t *testing.T) {
	now := time.Now()
	instances := []InstanceInfo{
		mkAggressive("app1", "oldest", time.Hour, 0, 2),
		mkAggressive("app1", "older", 45*time.Minute, 0, 2),
		mkAggressive("app1", "mid", 30*time.Minute, 0, 2),
		mkAggressive("app1", "newer", 15*time.Minute, 0, 2),
		mkAggressive("app1", "newest", 5*time.Minute, 0, 2),
	}
	got := ReapAggressive(now, instances, map[string]int{"app1": 0})
	if !equalSet(got, []string{"oldest", "older", "mid"}) {
		t.Fatalf("got %v, want [oldest older mid] (floor 2 + 3 extra)", got)
	}
}
