// sort_instance_infos_test.go — direct coverage for the unexported
// sortInstanceInfos helper in reader.go.
//
// The function is a pure insertion sort over []InstanceInfo by
// (Instance, Plan) lex. The existing reader_test.go only exercises
// it indirectly through Reader.Instances, which is gated behind
// runtime.GOOS == "linux" skips (cgroup v2 is Linux-only) — so the
// helper itself has zero coverage on macOS dev boxes and on
// non-Linux CI shards.
//
// This file hits the comparator branches directly:
//
//   - Instance primary ordering (b < a, b == a)
//   - Plan tiebreaker (only consulted when Instance == Instance)
//   - Empty input
//   - Already-sorted and reverse-sorted inputs
//   - Single-element slice
//
// Whitebox test (package cgroupstats) matching reader_test.go.
package cgroupstats

import (
	"sort"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
)

// TestSortInstanceInfos_Empty covers the zero-element case: the
// outer insertion-sort loop must not index into an empty slice.
func TestSortInstanceInfos_Empty(t *testing.T) {
	var s []InstanceInfo
	sortInstanceInfos(s)
	if len(s) != 0 {
		t.Errorf("empty sort: len = %d, want 0", len(s))
	}
}

// TestSortInstanceInfos_Single covers the single-element path: the
// outer loop's `i := 1` bound must not execute, the inner `j > 0`
// guard must not be hit.
func TestSortInstanceInfos_Single(t *testing.T) {
	s := []InstanceInfo{{Instance: "vm-x", Plan: api.PlanHobby}}
	sortInstanceInfos(s)
	if len(s) != 1 || s[0].Instance != "vm-x" {
		t.Errorf("single-element sort corrupted input: %+v", s)
	}
}

// TestSortInstanceInfos_AlreadySorted is the no-swap case: the
// comparator returns true (already in order) and the inner loop
// breaks on the first iteration. Confirms the early-break path.
func TestSortInstanceInfos_AlreadySorted(t *testing.T) {
	s := []InstanceInfo{
		{Instance: "vm-a", Plan: api.PlanHobby},
		{Instance: "vm-b", Plan: api.PlanHobby},
		{Instance: "vm-c", Plan: api.PlanHobby},
	}
	sortInstanceInfos(s)
	for i, want := range []string{"vm-a", "vm-b", "vm-c"} {
		if s[i].Instance != want {
			t.Errorf("already-sorted[%d] = %q, want %q", i, s[i].Instance, want)
		}
	}
}

// TestSortInstanceInfos_ReverseSorted exercises the swap path: every
// adjacent pair must be swapped once on the first inner iteration,
// then propagate outward. Hits the s[j-1], s[j] = s[j], s[j-1]
// branch in insertion sort.
func TestSortInstanceInfos_ReverseSorted(t *testing.T) {
	s := []InstanceInfo{
		{Instance: "vm-zzz", Plan: api.PlanHobby},
		{Instance: "vm-yyy", Plan: api.PlanHobby},
		{Instance: "vm-xxx", Plan: api.PlanHobby},
	}
	sortInstanceInfos(s)
	for i, want := range []string{"vm-xxx", "vm-yyy", "vm-zzz"} {
		if s[i].Instance != want {
			t.Errorf("reverse-sorted[%d] = %q, want %q", i, s[i].Instance, want)
		}
	}
}

// TestSortInstanceInfos_TiebreakByPlan hits the secondary-key path.
// Two entries share Instance="vm-dup"; the comparator must consult
// Plan to break the tie. Lower Plan string sorts first ("free" <
// "hobby" < "pro" < "scale" lex).
func TestSortInstanceInfos_TiebreakByPlan(t *testing.T) {
	s := []InstanceInfo{
		{Instance: "vm-dup", Plan: api.PlanScale},
		{Instance: "vm-dup", Plan: api.PlanFree},
		{Instance: "vm-dup", Plan: api.PlanPro},
		{Instance: "vm-dup", Plan: api.PlanHobby},
	}
	sortInstanceInfos(s)
	// After sort by (Instance, Plan), all share Instance so we order
	// by Plan only: free < hobby < pro < scale.
	wantPlans := []api.Plan{api.PlanFree, api.PlanHobby, api.PlanPro, api.PlanScale}
	for i, want := range wantPlans {
		if s[i].Plan != want {
			t.Errorf("tiebreak[%d].Plan = %q, want %q (full slice: %+v)",
				i, s[i].Plan, want, s)
		}
	}
}

// TestSortInstanceInfos_Mixed is a fuzz-like adversarial fixture:
// randomly ordered inputs (shuffled by hand with a deterministic
// sequence), sorted by (Instance, Plan), and compared against the
// canonical sort.Slice result. The two must agree for every input.
func TestSortInstanceInfos_Mixed(t *testing.T) {
	orig := []InstanceInfo{
		{Instance: "vm-c", Plan: api.PlanHobby},
		{Instance: "vm-a", Plan: api.PlanScale},
		{Instance: "vm-b", Plan: api.PlanFree},
		{Instance: "vm-a", Plan: api.PlanHobby},
		{Instance: "vm-c", Plan: api.PlanPro},
		{Instance: "vm-b", Plan: api.PlanHobby},
	}
	want := make([]InstanceInfo, len(orig))
	copy(want, orig)
	sort.Slice(want, func(i, j int) bool {
		if want[i].Instance != want[j].Instance {
			return want[i].Instance < want[j].Instance
		}
		return want[i].Plan < want[j].Plan
	})
	sortInstanceInfos(orig)
	for i := range want {
		if orig[i].Instance != want[i].Instance || orig[i].Plan != want[i].Plan {
			t.Errorf("[%d] got %+v, want %+v", i, orig[i], want[i])
		}
	}
}

// TestSortInstanceInfos_DoesNotMutateLength is a defensive pin: the
// insertion sort is in-place and must not allocate or grow the
// slice. Length must equal the input length.
func TestSortInstanceInfos_DoesNotMutateLength(t *testing.T) {
	in := []InstanceInfo{
		{Instance: "vm-x", Plan: api.PlanHobby},
		{Instance: "vm-y", Plan: api.PlanHobby},
		{Instance: "vm-z", Plan: api.PlanHobby},
	}
	sortInstanceInfos(in)
	if len(in) != 3 {
		t.Errorf("len mutated by sort: got %d, want 3", len(in))
	}
}
