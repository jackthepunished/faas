// Package preflight answers one question about a source tree: would this app
// run on Gregale? It is deliberately static — no build, no VM, no execution of
// customer code — so the answer is cheap enough to give an anonymous visitor.
//
// The verdict is intentionally conservative. A false green costs a user; a
// false amber costs a sentence of explanation.
package preflight

import "github.com/onebox-faas/faas/pkg/frameworkprofile"

// Level is the headline verdict for a source tree.
type Level string

const (
	// LevelGreen means the source already satisfies the container contract.
	LevelGreen Level = "green"
	// LevelAmber means the source runs once a declared change is supplied.
	LevelAmber Level = "amber"
	// LevelRed means a hard disqualifier from the container contract applies.
	LevelRed Level = "red"
)

// Finding is one actionable observation about the source tree. Detail says
// what was seen; Remedy says what to do about it.
type Finding struct {
	Code    string   `json:"code"`
	Level   Level    `json:"level"`
	Title   string   `json:"title"`
	Detail  string   `json:"detail"`
	Remedy  string   `json:"remedy,omitempty"`
	Sources []string `json:"sources,omitempty"`
}

// Verdict is the complete preflight answer for one source tree.
type Verdict struct {
	Level    Level                    `json:"level"`
	Findings []Finding                `json:"findings,omitempty"`
	Profile  frameworkprofile.Profile `json:"profile"`
}

// Evaluate maps an inferred profile onto the container contract. It considers
// only what static inference can see; contract disqualifiers that live in a
// Dockerfile or compose file are added by ScanContract.
func Evaluate(profile frameworkprofile.Profile) Verdict {
	verdict := Verdict{Level: LevelGreen, Profile: profile}
	for _, w := range profile.Warnings {
		finding := findingForWarning(w)
		verdict.Findings = append(verdict.Findings, finding)
		verdict.Level = worst(verdict.Level, finding.Level)
	}
	return verdict
}
