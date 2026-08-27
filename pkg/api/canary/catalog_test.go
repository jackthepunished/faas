package canary

import "testing"

func TestLookupPresetCatalog(t *testing.T) {
	tests := []struct {
		name        string
		canonical   string
		percentages []int
	}{
		{name: "none", canonical: "none", percentages: []int{100}},
		{name: "slow", canonical: "slow", percentages: []int{1, 100}},
		{name: "balanced", canonical: "balanced", percentages: []int{1, 10, 50, 100}},
		{name: "aggressive", canonical: "aggressive", percentages: []int{5, 50, 100}},
		{name: "1-10-50-100", canonical: "balanced", percentages: []int{1, 10, 50, 100}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := LookupPreset(tt.name)
			if !ok {
				t.Fatalf("LookupPreset(%q) returned ok=false", tt.name)
			}
			if got.Name != tt.canonical {
				t.Errorf("Name = %q, want %q", got.Name, tt.canonical)
			}
			if len(got.Percentages) != len(tt.percentages) {
				t.Fatalf("Percentages = %v, want %v", got.Percentages, tt.percentages)
			}
			for i, percentage := range tt.percentages {
				if got.Percentages[i] != percentage {
					t.Errorf("Percentages[%d] = %d, want %d", i, got.Percentages[i], percentage)
				}
			}
		})
	}
}

func TestLookupPresetRejectsUnknownPreset(t *testing.T) {
	if _, ok := LookupPreset("custom"); ok {
		t.Fatal("LookupPreset(custom) returned ok=true")
	}
}

func TestLookupPresetCopiesPercentages(t *testing.T) {
	got, ok := LookupPreset("balanced")
	if !ok {
		t.Fatal("LookupPreset(balanced) returned ok=false")
	}
	got.Percentages[0] = 99

	again, ok := LookupPreset("balanced")
	if !ok {
		t.Fatal("second LookupPreset(balanced) returned ok=false")
	}
	if again.Percentages[0] != 1 {
		t.Fatalf("catalog was mutated through returned slice: %v", again.Percentages)
	}
}
