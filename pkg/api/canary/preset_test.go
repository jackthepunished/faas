// pkg/api/canary/preset_test.go — pins the catalog closed-set, the
// lookup-by-name contract, the stage accessors, and Validate's
// terminal-stage invariant. The catalog is platform-owned; a typo
// here surfaces as "1% deployment forever" or a panic-on-metd-startup,
// so the test suite pins the layout to disk.
//
// Build tag mirrors pkg/api/alerts_test.go convention (no build tag
// required — pkg/api canary is a pure-Go package with no DB deps).

package canary

import (
	"testing"
	"time"
)

func TestAllowedCanaryPresets_ClosedSet(t *testing.T) {
	want := []string{"none", "slow", "balanced", "aggressive", "1-10-50-100"}
	if len(AllowedCanaryPresets) != len(want) {
		t.Fatalf("AllowedCanaryPresets = %v, want %v", AllowedCanaryPresets, want)
	}
	for i, n := range want {
		if AllowedCanaryPresets[i] != n {
			t.Errorf("AllowedCanaryPresets[%d] = %q, want %q", i, AllowedCanaryPresets[i], n)
		}
	}
}

func TestAllowedCanaryPreset_Membership(t *testing.T) {
	for _, n := range AllowedCanaryPresets {
		if !AllowedCanaryPreset(n) {
			t.Errorf("AllowedCanaryPreset(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"", "SLOW", "balance", "1-10-100", "x", "0"} {
		if AllowedCanaryPreset(n) {
			t.Errorf("AllowedCanaryPreset(%q) = true, want false", n)
		}
	}
}

func TestLookupPreset_Slow(t *testing.T) {
	p, ok := LookupPreset("slow")
	if !ok {
		t.Fatal("LookupPreset(\"slow\") = ok=false")
	}
	wantStages := []Stage{
		{Percent: 1, Duration: 5 * time.Minute},
		{Percent: 10, Duration: 5 * time.Minute},
		{Percent: 100, Duration: 0},
	}
	if len(p.Stages) != len(wantStages) {
		t.Fatalf("slow stages = %d, want %d (%v)", len(p.Stages), len(wantStages), p.Stages)
	}
	for i, s := range wantStages {
		if p.Stages[i] != s {
			t.Errorf("slow stage %d = %+v, want %+v", i, p.Stages[i], s)
		}
	}
	if p.TotalSteps() != 3 {
		t.Errorf("slow TotalSteps = %d, want 3", p.TotalSteps())
	}
}

func TestLookupPreset_Balanced(t *testing.T) {
	p, ok := LookupPreset("balanced")
	if !ok {
		t.Fatal("LookupPreset(\"balanced\") = ok=false")
	}
	if len(p.Stages) != 4 {
		t.Fatalf("balanced stages = %d, want 4", len(p.Stages))
	}
	wantPercents := []int{1, 10, 50, 100}
	for i, want := range wantPercents {
		if p.Stages[i].Percent != want {
			t.Errorf("balanced stage %d percent = %d, want %d", i, p.Stages[i].Percent, want)
		}
	}
	if p.Stages[3].Duration != 0 {
		t.Errorf("balanced terminal stage duration = %v, want 0", p.Stages[3].Duration)
	}
}

func TestLookupPreset_OneTenFiftyHundredAliasOfBalanced(t *testing.T) {
	// The CLI's --canary-preset 1-10-50-100 must resolve to the
	// same ladder as `balanced` so the catalog has a single
	// canonical set of Stages (operators editing one shouldn't
	// fork the layout).
	alias, ok1 := LookupPreset("1-10-50-100")
	balanced, ok2 := LookupPreset("balanced")
	if !ok1 || !ok2 {
		t.Fatalf("lookup ok=%v,%v; both must succeed", ok1, ok2)
	}
	if len(alias.Stages) != len(balanced.Stages) {
		t.Fatalf("alias Stages=%d, balanced Stages=%d; must match", len(alias.Stages), len(balanced.Stages))
	}
	for i := range alias.Stages {
		if alias.Stages[i] != balanced.Stages[i] {
			t.Fatalf("stage %d: alias=%+v balanced=%+v; must match", i, alias.Stages[i], balanced.Stages[i])
		}
	}
}

func TestLookupPreset_None(t *testing.T) {
	p, ok := LookupPreset("none")
	if !ok {
		t.Fatal("LookupPreset(\"none\") = ok=false")
	}
	if len(p.Stages) != 0 {
		t.Errorf("none stages = %d, want 0 (the disable case)", len(p.Stages))
	}
	if p.TotalSteps() != 0 {
		t.Errorf("none TotalSteps = %d, want 0", p.TotalSteps())
	}
}

func TestLookupPreset_Unknown(t *testing.T) {
	for _, n := range []string{"", "fast", "x", "SLOW", "balance"} {
		if _, ok := LookupPreset(n); ok {
			t.Errorf("LookupPreset(%q) = ok=true, want false", n)
		}
	}
}

func TestLookupPreset_ReturnsCopy(t *testing.T) {
	a, _ := LookupPreset("slow")
	a.Stages[0].Percent = 99
	b, _ := LookupPreset("slow")
	if b.Stages[0].Percent != 1 {
		t.Errorf("catalog must return a copy; second lookup saw mutation: Stages[0].Percent=%d", b.Stages[0].Percent)
	}
}

func TestPresetStageAt(t *testing.T) {
	p, _ := LookupPreset("slow")
	cases := []struct {
		step   int
		wantOk bool
	}{
		{0, true},
		{2, true},
		{3, false}, // past last
		{-1, false},
	}
	for _, tc := range cases {
		_, ok := p.StageAt(tc.step)
		if ok != tc.wantOk {
			t.Errorf("StageAt(%d) ok=%v, want %v", tc.step, ok, tc.wantOk)
		}
	}
}

func TestPresetValidate(t *testing.T) {
	for _, name := range AllowedCanaryPresets {
		p, ok := LookupPreset(name)
		if !ok {
			t.Errorf("LookupPreset(%q) = ok=false in catalog", name)
			continue
		}
		if err := p.Validate(); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", name, err)
		}
	}
}

func TestPresetValidate_TerminalMustReach100(t *testing.T) {
	bad := Preset{Name: "broken", Stages: []Stage{
		{Percent: 1, Duration: time.Minute},
		{Percent: 50, Duration: 0}, // terminal but only 50%
	}}
	if err := bad.Validate(); err == nil {
		t.Error("Validate accepted non-100 terminal; want error")
	}
}

func TestPresetValidate_OutOfRangePercent(t *testing.T) {
	bad := Preset{Name: "broken", Stages: []Stage{
		{Percent: -1, Duration: time.Minute},
		{Percent: 100, Duration: 0},
	}}
	if err := bad.Validate(); err == nil {
		t.Error("Validate accepted negative percent; want error")
	}
}

func TestPresetValidate_NegativeDuration(t *testing.T) {
	bad := Preset{Name: "broken", Stages: []Stage{
		{Percent: 1, Duration: -time.Minute},
		{Percent: 100, Duration: 0},
	}}
	if err := bad.Validate(); err == nil {
		t.Error("Validate accepted negative duration; want error")
	}
}
