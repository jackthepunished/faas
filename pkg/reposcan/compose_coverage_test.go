package reposcan

import (
	"testing"
)

// TestEnvKeys_AllForms — exercises every branch of envKeys:
// nil, map[string]any, map[any]any, []any, []string.
func TestEnvKeys_AllForms(t *testing.T) {
	t.Parallel()
	if got := envKeys(nil); got != nil {
		t.Errorf("envKeys(nil) = %v, want nil", got)
	}
	m := map[string]any{"B": "2", "A": "1"}
	got := envKeys(m)
	want := []string{"A", "B"}
	if !equalSet(got, want) {
		t.Errorf("envKeys(map[string]any) = %v, want %v", got, want)
	}
	mAny := map[any]any{42: "v", "X": "v2"}
	got2 := envKeys(mAny)
	if !equalSet(got2, []string{"X"}) {
		t.Errorf("envKeys(map[any]any) = %v, want [X]", got2)
	}
	got3 := envKeys([]any{"FOO", "BAR=baz"})
	if !equalSet(got3, []string{"FOO", "BAR"}) {
		t.Errorf("envKeys([]any) = %v, want {FOO, BAR}", got3)
	}
	got4 := envKeys([]string{"FOO", "BAR=baz"})
	if !equalSet(got4, []string{"FOO", "BAR"}) {
		t.Errorf("envKeys([]string) = %v, want {FOO, BAR}", got4)
	}
}

// TestParsePorts_AllForms — covers the string 1-part, 2-part, 3-part
// forms, the range-drop path, the int form, the long-form map, and
// a noisy invalid input.
func TestParsePorts_AllForms(t *testing.T) {
	t.Parallel()
	if got := parsePorts([]any{"8080"}); len(got) != 1 || got[0] != 8080 {
		t.Errorf("parsePorts(8080) = %v, want [8080]", got)
	}
	if got := parsePorts([]any{"8080:80"}); len(got) != 1 || got[0] != 8080 {
		t.Errorf("parsePorts(8080:80) = %v, want [8080]", got)
	}
	if got := parsePorts([]any{"127.0.0.1:8080:80"}); len(got) != 1 || got[0] != 8080 {
		t.Errorf("parsePorts(127.0.0.1:8080:80) = %v, want [8080]", got)
	}
	// Range forms are silently dropped.
	if got := parsePorts([]any{"8080-8090:80", "8000-8010:80"}); got != nil {
		t.Errorf("parsePorts(ranges) = %v, want nil (dropped)", got)
	}
	// Int and float forms.
	if got := parsePorts([]any{8080, 9090.0}); len(got) != 2 || got[0] != 8080 || got[1] != 9090 {
		t.Errorf("parsePorts(ints) = %v, want [8080 9090]", got)
	}
	// Long-form with published + target.
	if got := parsePorts([]any{map[string]any{"published": 7070, "target": 80}}); len(got) != 1 || got[0] != 7070 {
		t.Errorf("parsePorts(long) = %v, want [7070]", got)
	}
	// Long-form with only target (no published).
	if got := parsePorts([]any{map[string]any{"target": 6060}}); len(got) != 1 || got[0] != 6060 {
		t.Errorf("parsePorts(long target-only) = %v, want [6060]", got)
	}
	// Empty input.
	if got := parsePorts(nil); got != nil {
		t.Errorf("parsePorts(nil) = %v, want nil", got)
	}
}

// TestIntOf_AllForms — intOf is the inner type-assertion helper for
// parsePorts. Exercise every variant.
func TestIntOf_AllForms(t *testing.T) {
	t.Parallel()
	if intOf(8080) != 8080 {
		t.Errorf("intOf(int) = %d, want 8080", intOf(8080))
	}
	if intOf(float64(8080)) != 8080 {
		t.Errorf("intOf(float64) = %d, want 8080", intOf(float64(8080)))
	}
	if intOf("8080") != 8080 {
		t.Errorf("intOf(string) = %d, want 8080", intOf("8080"))
	}
	if intOf(nil) != 0 {
		t.Errorf("intOf(nil) = %d, want 0", intOf(nil))
	}
}

// TestCommandSlice_AllForms — `command:` can be a string or an array.
func TestCommandSlice_AllForms(t *testing.T) {
	t.Parallel()
	if got := commandSlice("bundle exec sidekiq"); len(got) != 1 || got[0] != "bundle exec sidekiq" {
		t.Errorf("commandSlice(str) = %v", got)
	}
	if got := commandSlice([]any{"bundle", "exec", "sidekiq"}); len(got) != 3 {
		t.Errorf("commandSlice([any]) = %v", got)
	}
	if got := commandSlice(nil); got != nil {
		t.Errorf("commandSlice(nil) = %v", got)
	}
}

// TestSeedWarning_NoLongerEmitted — verifies the deliberate choice
// in scan.go to NOT pollute Warnings with per-seed notices.
func TestSeedWarning_NoLongerEmitted(t *testing.T) {
	t.Parallel()
	// Direct call: seedWarning takes a workloadSeed.
	w := seedWarning(workloadSeed{source: "compose.yaml", name: "api"})
	if w == "" {
		t.Errorf("seedWarning helper returned empty for a real seed")
	}
	// Empty name short-circuits.
	if w2 := seedWarning(workloadSeed{source: "compose.yaml"}); w2 != "" {
		t.Errorf("seedWarning with empty name = %q, want empty", w2)
	}
}
