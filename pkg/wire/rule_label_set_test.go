package wire

import (
	"fmt"
	"sync"
	"testing"
)

// Tests for the per-app admission set backing the
// gateway_validate_failures_total{app_id, rule_id, mode, reason}
// metric (ADR-128 §D3). Mirrors the boxLabelSet / accountLabelSet
// test pattern (pkg/wire/metrics_cardinality_test.go).

// TestRuleLabel_FirstNPerAppAdmitted asserts that the first
// ruleLabelSetCap distinct rule IDs are admitted verbatim on the
// same app. The 257th distinct rule ID is what the next test
// pins.
func TestRuleLabel_FirstNPerAppAdmitted(t *testing.T) {
	s := newRuleLabelSet()
	const app = "00000000-0000-0000-0000-000000000001"
	for i := 0; i < ruleLabelSetCap; i++ {
		got := s.admit(app, fmt.Sprintf("rule-%04d", i))
		if got != fmt.Sprintf("rule-%04d", i) {
			t.Fatalf("rule %d admitted as %q, want verbatim %q (the first %d distinct rule IDs must be admitted)",
				i, got, fmt.Sprintf("rule-%04d", i), ruleLabelSetCap)
		}
	}
}

// TestRuleLabel_OverflowsToOtherPerApp asserts that the
// (ruleLabelSetCap+1)th distinct rule ID on the same app
// collapses to otherRuleLabel, while a separate app still has
// its full cap available.
func TestRuleLabel_OverflowsToOtherPerApp(t *testing.T) {
	s := newRuleLabelSet()
	const appA = "00000000-0000-0000-0000-00000000000a"
	const appB = "00000000-0000-0000-0000-00000000000b"

	for i := 0; i < ruleLabelSetCap; i++ {
		s.admit(appA, fmt.Sprintf("rule-%04d", i))
	}
	// (cap+1)th rule on appA overflows.
	if got := s.admit(appA, "overflow-rule"); got != otherRuleLabel {
		t.Errorf("overflow on appA: got %q, want %q", got, otherRuleLabel)
	}
	// A separate app must still admit its first rule verbatim
	// — the per-app cap is independent across apps.
	if got := s.admit(appB, "first-rule"); got != "first-rule" {
		t.Errorf("appB first rule: got %q, want verbatim %q (per-app cap must be independent across apps)",
			got, "first-rule")
	}
	// And another overflow on appA stays collapsed.
	if got := s.admit(appA, "another-overflow"); got != otherRuleLabel {
		t.Errorf("second overflow on appA: got %q, want %q", got, otherRuleLabel)
	}
}

// TestRuleLabel_ReservedValuesDontConsumeCapacity asserts that
// otherRuleLabel is admitted without consuming the user-facing
// cap. Without this, the reserved bucket would steal a slot
// from the first 256 real rules.
func TestRuleLabel_ReservedValuesDontConsumeCapacity(t *testing.T) {
	s := newRuleLabelSet()
	const app = "00000000-0000-0000-0000-000000000002"
	// Pre-admit the reserved value first.
	if got := s.admit(app, otherRuleLabel); got != otherRuleLabel {
		t.Fatalf("reserved otherRuleLabel: got %q, want %q", got, otherRuleLabel)
	}
	// The cap should still hold ruleLabelSetCap distinct real
	// rules, not ruleLabelSetCap-1.
	for i := 0; i < ruleLabelSetCap; i++ {
		got := s.admit(app, fmt.Sprintf("real-rule-%04d", i))
		if got != fmt.Sprintf("real-rule-%04d", i) {
			t.Fatalf("rule %d (with reserved pre-admitted): got %q, want verbatim — the reserved bucket must not consume capacity",
				i, got)
		}
	}
}

// TestRuleLabel_EmptyRuleIDCollapsesToOther asserts that an
// empty rule_id (pathological — the runtime always supplies
// one) collapses to otherRuleLabel so the Prometheus output
// never carries an empty string in the rule_id label position.
func TestRuleLabel_EmptyRuleIDCollapsesToOther(t *testing.T) {
	s := newRuleLabelSet()
	if got := s.admit("any-app", ""); got != otherRuleLabel {
		t.Errorf("empty ruleID: got %q, want %q", got, otherRuleLabel)
	}
}

// TestRuleLabel_AlreadyAdmittedIsStable asserts that a second
// admit call for an already-admitted (appID, ruleID) pair
// returns the same value verbatim — the lookup path is the
// hot path and must be O(1) and free of capacity side effects.
func TestRuleLabel_AlreadyAdmittedIsStable(t *testing.T) {
	s := newRuleLabelSet()
	const app = "00000000-0000-0000-0000-000000000003"
	const rule = "stable-rule-id"
	first := s.admit(app, rule)
	second := s.admit(app, rule)
	if first != rule || second != rule {
		t.Errorf("already-admitted: first=%q second=%q, want %q", first, second, rule)
	}
}

// TestRuleLabel_ConcurrentAdmission asserts that the admission
// set is race-free under N parallel goroutines admitting
// overlapping (appID, ruleID) pairs. Property: the final
// admitted set is a subset of the input set, and no panic.
func TestRuleLabel_ConcurrentAdmission(t *testing.T) {
	s := newRuleLabelSet()
	const app = "00000000-0000-0000-0000-000000000004"
	const goroutines = 32
	const perGoroutine = 64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				// Half overlap, half distinct to stress both
				// the already-admitted lookup and the cap
				// check.
				var ruleID string
				if i%2 == 0 {
					ruleID = fmt.Sprintf("shared-%04d", i)
				} else {
					ruleID = fmt.Sprintf("g%02d-i%04d", g, i)
				}
				_ = s.admit(app, ruleID)
			}
		}(g)
	}
	wg.Wait()
	// Sanity: re-admitting the same shared rules returns
	// verbatim (no race-induced overflow collapse).
	for i := 0; i < perGoroutine; i += 2 {
		ruleID := fmt.Sprintf("shared-%04d", i)
		if got := s.admit(app, ruleID); got != ruleID {
			t.Errorf("shared rule %q after concurrent admit: got %q, want verbatim", ruleID, got)
		}
	}
}

// TestRuleLabel_PropertyFuzzed admission is bounded — pins
// the load-bearing property for the metric cardinality budget
// (ADR-128 §D3): 10k fuzzed distinct rule IDs on the same app
// must result in exactly ruleLabelSetCap distinct real
// admissions + 1 reserved (otherRuleLabel). Anything more
// blows the Prometheus per-instance label budget.
func TestRuleLabel_PropertyFuzzed(t *testing.T) {
	s := newRuleLabelSet()
	const app = "00000000-0000-0000-0000-000000000005"
	const fuzzed = 10_000
	// Track the OBSERVED labels — what the caller would emit
	// to Prometheus. Distinct input rule IDs that collapsed to
	// otherRuleLabel all converge on the same observed label,
	// so the set cardinality reflects what the metric
	// actually exposes, not the input shape.
	observed := make(map[string]struct{}, ruleLabelSetCap+1)
	for i := 0; i < fuzzed; i++ {
		ruleID := fmt.Sprintf("fuzz-%08d", i)
		got := s.admit(app, ruleID)
		observed[got] = struct{}{}
	}
	// Expected: exactly ruleLabelSetCap real rule IDs admitted
	// verbatim, plus the reserved otherRuleLabel.
	want := ruleLabelSetCap + 1
	if len(observed) != want {
		t.Errorf("distinct OBSERVED labels after 10k fuzz: got %d, want %d (cap=%d real + 1 reserved)",
			len(observed), want, ruleLabelSetCap)
	}
}
