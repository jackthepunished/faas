// sbom_edge_test.go — additive edge-case coverage for pkg/releaseinstall/sbom.go.
//
// pkg/releaseinstall/sbom_test.go (existing) covers the load-bearing
// paths (Parse SPDX happy / CycloneDX rejection / empty / non-CVE
// annotations / CRITICAL+ HIGH regression / nil baseline / round-trip).
// This file fills the leftover branch-coverage gaps left by that
// suite: the String() default branch, the severityFromString switch
// defaults, the CVERegression error format, the KGVZero default
// fields, the ReadBaseline empty-git_sha precondition, and the
// formatRegressions multi-regression path.
//
// External test package (package releaseinstall_test) matching the
// existing sbom_test.go convention.
package releaseinstall_test

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/releaseinstall"
)

// TestSBOMSeverity_String covers the String() switch on
// SBOMSeverity. Critical + High + Medium + Low are the documented
// buckets; Unknown is the parser's default for unrecognised strings
// and must serialise as "UNKNOWN" (used in operator-facing error
// messages and dashboards).
func TestSBOMSeverity_String(t *testing.T) {
	cases := []struct {
		sev  releaseinstall.SBOMSeverity
		want string
	}{
		{releaseinstall.SevCritical, "CRITICAL"},
		{releaseinstall.SevHigh, "HIGH"},
		{releaseinstall.SevMedium, "MEDIUM"},
		{releaseinstall.SevLow, "LOW"},
		{releaseinstall.SevUnknown, "UNKNOWN"},
		// Out-of-band int values (not 0–4) must hit the default.
		{releaseinstall.SBOMSeverity(99), "UNKNOWN"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.sev.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCVERegression_ErrorFormat pins the exact error shape used in
// operator dashboards. The format MUST stay stable so existing
// runbooks that grep for "regression: N → M (delta +K)" continue to
// match.
func TestCVERegression_ErrorFormat(t *testing.T) {
	r := releaseinstall.CVERegression{
		Severity: releaseinstall.SevCritical,
		PrevN:    2, NewN: 5, Delta: 3,
	}
	got := r.Error()
	want := "CRITICAL regression: 2 → 5 (delta +3)"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// TestSeverityFromString_Cases is the branch table for the
// unexported parser helper. Covers each severity + the default +
// canonicalisation cases (ToUpper + TrimSpace). The parser is
// case-insensitive and whitespace-tolerant on purpose — syft's
// SPDX emitter has drifted casing across versions.
func TestSeverityFromString_Cases(t *testing.T) {
	// We test severityFromString indirectly via ParseSPDXv2_3:
	// the helper is unexported, but every branch maps to a
	// distinct SBOMCounts.X increment which we can observe.
	cases := []struct {
		label    string
		severity string
		want     releaseinstall.SBOMSeverity
	}{
		{"upper_critical", "CRITICAL", releaseinstall.SevCritical},
		{"lower_critical", "critical", releaseinstall.SevCritical},
		{"mixed_critical", "Critical", releaseinstall.SevCritical},
		{"whitespace_critical", "  CRITICAL  ", releaseinstall.SevCritical},
		{"upper_high", "HIGH", releaseinstall.SevHigh},
		{"upper_medium", "MEDIUM", releaseinstall.SevMedium},
		{"upper_low", "LOW", releaseinstall.SevLow},
		{"unknown_string", "FOO", releaseinstall.SevUnknown},
		{"empty_string", "", releaseinstall.SevUnknown},
		{"whitespace_only", "   ", releaseinstall.SevUnknown},
		{"garbage", "lolnotasev", releaseinstall.SevUnknown},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.label, func(t *testing.T) {
			doc := map[string]any{
				"spdxVersion": "SPDX-2.3",
				"packages": []any{
					map[string]any{
						"annotations": []any{
							map[string]string{
								"severity": tc.severity,
								"comment":  "CVE",
							},
						},
					},
				},
			}
			body := mustJSONForSBOMEdgeTest(t, doc)
			c, err := releaseinstall.ParseSPDXv2_3(body)
			if err != nil {
				t.Fatalf("ParseSPDXv2_3: %v", err)
			}
			switch tc.want {
			case releaseinstall.SevCritical:
				if c.CriticalN != 1 {
					t.Errorf("CriticalN = %d, want 1", c.CriticalN)
				}
			case releaseinstall.SevHigh:
				if c.HighN != 1 {
					t.Errorf("HighN = %d, want 1", c.HighN)
				}
			case releaseinstall.SevMedium:
				if c.MediumN != 1 {
					t.Errorf("MediumN = %d, want 1", c.MediumN)
				}
			case releaseinstall.SevLow:
				if c.LowN != 1 {
					t.Errorf("LowN = %d, want 1", c.LowN)
				}
			case releaseinstall.SevUnknown:
				if c.CriticalN != 0 || c.HighN != 0 || c.MediumN != 0 || c.LowN != 0 {
					t.Errorf("Unknown severity must not increment any bucket, got %+v", c)
				}
			}
		})
	}
}

// TestKGVZero_Defaults asserts the conservative default KGV fields.
// The first install on a fresh box MUST have CRITICAL=0, HIGH=0;
// the package-level invariant is "Diff with KGVZero and any non-zero
// count returns ErrCVERegression" — already covered by
// TestSBOMBaseline_DiffDetectsCriticalRegression in sbom_test.go,
// but pinning the field defaults here documents the contract.
func TestKGVZero_Defaults(t *testing.T) {
	b := releaseinstall.KGVZero("0123456789abcdef0123456789abcdef01234567")
	if b.GitSHA != "0123456789abcdef0123456789abcdef01234567" {
		t.Errorf("GitSHA = %q, want gitSHA passthrough", b.GitSHA)
	}
	if b.Counts.CriticalN != 0 || b.Counts.HighN != 0 ||
		b.Counts.MediumN != 0 || b.Counts.LowN != 0 {
		t.Errorf("KGVZero counts = %+v, want all zero", b.Counts)
	}
	if b.CreatedAt == "" {
		t.Errorf("KGVZero CreatedAt = empty; the RFC3339 default sentinel must be present")
	}
}

// TestReadBaseline_EmptyGitSHARefused covers the precondition
// guard: ReadBaseline refuses empty git_sha before touching the
// filesystem. The error is intentionally non-ErrNilBaseline so
// callers can distinguish "bad arg" from "no baseline".
func TestReadBaseline_EmptyGitSHARefused(t *testing.T) {
	root := t.TempDir()
	_, err := releaseinstall.ReadBaseline(root, "")
	if err == nil {
		t.Fatal("empty git_sha must be rejected")
	}
	// We don't pin the exact text, but it must not be ErrNilBaseline —
	// the precondition guard fires before the fs check, so "missing"
	// is the wrong diagnosis.
	if errors.Is(err, releaseinstall.ErrNilBaseline) {
		t.Errorf("ReadBaseline(empty) returned ErrNilBaseline; should be a precondition error: %v", err)
	}
}

// TestDiff_MultipleRegressions orders CRITICAL first in the error
// message. This is the operator runbook contract — the worst
// regression always leads.
func TestDiff_MultipleRegressions(t *testing.T) {
	base := releaseinstall.SBOMBaseline{
		GitSHA: "0123456789abcdef0123456789abcdef01234567",
		Counts: releaseinstall.SBOMCounts{CriticalN: 1, HighN: 1},
	}
	// Both CRITICAL AND HIGH regress by 2 each.
	current := releaseinstall.SBOMCounts{CriticalN: 3, HighN: 3}

	regs, err := base.Diff(current)
	if !errors.Is(err, releaseinstall.ErrCVERegression) {
		t.Fatalf("Diff with two regressions: got %v, want ErrCVERegression", err)
	}
	if len(regs) != 2 {
		t.Fatalf("len(regs) = %d, want 2", len(regs))
	}
	// Most-severe first: CRITICAL (enum 1) before HIGH (enum 2),
	// because enum order is most-severe-to-least.
	if regs[0].Severity != releaseinstall.SevCritical {
		t.Errorf("regs[0].Severity = %v, want CRITICAL (most severe leads)", regs[0].Severity)
	}
	if regs[1].Severity != releaseinstall.SevHigh {
		t.Errorf("regs[1].Severity = %v, want HIGH", regs[1].Severity)
	}
	// formatRegressions: "; " separator between entries.
	if msg := err.Error(); !strings.Contains(msg, "; ") {
		t.Errorf("multi-regression error must use '; ' separator, got: %v", err)
	}
	re := regexp.MustCompile(`CRITICAL regression: \d+ → \d+ \(delta \+\d+\); HIGH regression: \d+ → \d+ \(delta \+\d+\)`)
	if !re.MatchString(err.Error()) {
		t.Errorf("multi-regression error format wrong, got: %v", err)
	}
}

// TestDiff_NoRegressionsEvenWithMixedChanges asserts that HIGH
// staying even and MEDIUM/LOW rising is not an ErrCVERegression —
// the gate only fires on CRITICAL or HIGH growth.
func TestDiff_NoRegressionsEvenWithMixedChanges(t *testing.T) {
	base := releaseinstall.SBOMBaseline{
		GitSHA: "0123456789abcdef0123456789abcdef01234567",
		Counts: releaseinstall.SBOMCounts{
			CriticalN: 0, HighN: 0, MediumN: 1, LowN: 1,
		},
	}
	// CRITICAL stays 0; HIGH stays 0; MEDIUM +LOW grow.
	current := releaseinstall.SBOMCounts{
		CriticalN: 0, HighN: 0, MediumN: 100, LowN: 50,
	}

	regs, err := base.Diff(current)
	if err != nil {
		t.Fatalf("Diff with medium/low growth only: got %v, want nil", err)
	}
	if len(regs) != 0 {
		t.Fatalf("regs = %+v, want empty (MEDIUM/LOW don't trigger the gate)", regs)
	}
}

// TestDiff_OnlyEquals asserts the trivially-equal case: the new SBoM
// has the exact same CRITICAL/HIGH counts as the baseline. No
// regressions, no error.
func TestDiff_OnlyEquals(t *testing.T) {
	base := releaseinstall.SBOMBaseline{
		GitSHA: "0123456789abcdef0123456789abcdef01234567",
		Counts: releaseinstall.SBOMCounts{CriticalN: 4, HighN: 7, MediumN: 10, LowN: 30},
	}
	current := base.Counts // exact copy

	regs, err := base.Diff(current)
	if err != nil {
		t.Errorf("Diff with equal counts: got %v, want nil", err)
	}
	if len(regs) != 0 {
		t.Errorf("regs = %+v, want empty", regs)
	}
}

func mustJSONForSBOMEdgeTest(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
