package imaged

// grype_test.go — behavior-parity pins for the PR-2 typed
// ScanResult refactor (issue #464 / ADR-055). The plan called
// for a behavior-parity assertion that the legacy base-ext4
// sidecar JSON shape is byte-identical after the
// map[string]int → *ScanResult refactor. The round-trip
// through TestEnsureBaseExt4 already covers the integration
// path; this file pins the two small new primitives:
//
//   1. bumpSeverity: a normalised severity string increments
//      the right bucket; an unknown severity collapses to
//      UNKNOWN (the existing normalizeGrypeSeverity default).
//
//   2. toMap: a *ScanResult projects back into the
//      `map[string]int` shape the legacy sidecar JSON expects,
//      with the closed-enum keys (CRITICAL/HIGH/MEDIUM/LOW/
//      UNKNOWN) in the canonical order. nil receiver → nil
//      map (so the sidecar-write site can short-circuit on a
//      missing scan without panicking on a nil deref).
//
// A failure here means a future refactor drifts the sidecar
// contract; vmmd's bringUpScanCheck would silently miss
// CRITICAL findings on the next staged base.

import "testing"

// TestScanResult_BumpSeverity pins the per-bucket increment
// against the closed-enum Severity* constants. The exact
// count for an unknown severity is UNKNOWN (matches the
// existing normalizeGrypeSeverity default at grype.go:118).
func TestScanResult_BumpSeverity(t *testing.T) {
	cases := []struct {
		severity string
		want     SeverityCounts
	}{
		{SeverityCritical, SeverityCounts{Critical: 1}},
		{SeverityHigh, SeverityCounts{High: 1}},
		{SeverityMedium, SeverityCounts{Medium: 1}},
		{SeverityLow, SeverityCounts{Low: 1}},
		{SeverityUnknown, SeverityCounts{Unknown: 1}},
		// Unknown / malformed severity collapses to UNKNOWN.
		{"", SeverityCounts{Unknown: 1}},
		{"bogus", SeverityCounts{Unknown: 1}},
		// Negligible is upper-cased by normalizeGrypeSeverity
		// before bumpSeverity is called, but a direct
		// bumpSeverity with "NEGLIGIBLE" still collapses to
		// UNKNOWN — the collapse happens at the
		// normalizeGrypeSeverity layer, not here. The pin is
		// for the direct call only.
		{"NEGLIGIBLE", SeverityCounts{Unknown: 1}},
	}
	for _, c := range cases {
		var got ScanResult
		got.bumpSeverity(c.severity)
		if got.SeverityCounts != c.want {
			t.Errorf("bumpSeverity(%q) = %+v, want %+v", c.severity, got.SeverityCounts, c.want)
		}
	}
}

// TestScanResult_ToMap pins the projection back to the
// legacy sidecar JSON shape. The keys MUST be the uppercase
// closed-enum Severity* constants so the sidecar JSON
// contract is byte-identical to the pre-PR-2 output for any
// given input. nil receiver → nil map (the sidecar-write
// site uses this to short-circuit on a missing scan).
func TestScanResult_ToMap(t *testing.T) {
	s := &ScanResult{
		SeverityCounts: SeverityCounts{
			Critical: 3, High: 2, Medium: 1, Low: 0, Unknown: 0,
		},
	}
	got := s.toMap()
	want := map[string]int{
		SeverityCritical: 3,
		SeverityHigh:     2,
		SeverityMedium:   1,
		SeverityLow:      0,
		SeverityUnknown:  0,
	}
	if len(got) != len(want) {
		t.Errorf("toMap returned %d keys, want %d", len(got), len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("toMap[%q] = %d, want %d", k, got[k], v)
		}
	}
}

// TestScanResult_ToMap_NilReceiver pins the nil-safe short
// circuit. The sidecar-write site at base_stage.go reads
// `findings.toMap()` after a nil-check; a nil receiver that
// panics would surface as a build-pipeline crash instead of
// the expected fail-closed CRITICAL=9999 placeholder path.
func TestScanResult_ToMap_NilReceiver(t *testing.T) {
	var s *ScanResult
	if got := s.toMap(); got != nil {
		t.Errorf("nil receiver: toMap() = %v, want nil", got)
	}
}
