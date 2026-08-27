// Package canary contains the closed-set rollout preset catalog shared by
// API validation and rollout orchestration.
package canary

// Preset describes one supported traffic progression. Percentages are the
// traffic assigned to the new deployment at each step, including the final
// 100% promotion step.
type Preset struct {
	Name        string
	Percentages []int
}

var presets = map[string]Preset{
	"none": {
		Name:        "none",
		Percentages: []int{100},
	},
	"slow": {
		Name:        "slow",
		Percentages: []int{1, 100},
	},
	"balanced": {
		Name:        "balanced",
		Percentages: []int{1, 10, 50, 100},
	},
	"aggressive": {
		Name:        "aggressive",
		Percentages: []int{5, 50, 100},
	},
}

// AllowedCanaryPresets is the stable validation order exposed in API error
// messages and CLI help. The numeric spelling is kept as a backwards-
// compatible alias for the balanced ladder.
var AllowedCanaryPresets = []string{
	"none",
	"slow",
	"balanced",
	"aggressive",
	"1-10-50-100",
}

// LookupPreset resolves a user-facing preset name. The numeric balanced
// ladder is accepted as an alias but returns the canonical "balanced" name.
// The returned percentage slice is copied so callers cannot mutate the
// process-wide catalog.
func LookupPreset(name string) (Preset, bool) {
	if name == "1-10-50-100" {
		name = "balanced"
	}
	preset, ok := presets[name]
	if !ok {
		return Preset{}, false
	}
	preset.Percentages = append([]int(nil), preset.Percentages...)
	return preset, true
}
