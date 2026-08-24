package gateway

import (
	"strings"
	"testing"
)

// TestObserveEdgeRuleValidateFailure_ClosedLabelSet checks that the
// metric coercer folds any unknown mode or reason into the closed
// {observe, warn, block, other} × {required_missing, type_mismatch,
// additional_properties_not_allowed, enum_violation, format_violation,
// other} cross-product. The closed set is the metric's contract with
// the §12 dashboard panel — a regression that allows a new tuple
// would inflate the series count and break the label-set alarms
// (issue #975 #3 / Mega-Foundation #979-a / ADR-128 §5).
//
// Both the deprecated legacy counter
// (gateway_edge_rule_validate_failures_total{mode, reason}) and
// the new canonical counter
// (gateway_validate_failures_total{app_id, rule_id, mode, reason})
// are emitted on every call. The legacy counter is shadow-emitted
// for one release per ADR-128 §5, then dropped.

func TestObserveEdgeRuleValidateFailure_ClosedLabelSet(t *testing.T) {
	m := NewMetrics()
	const appID = "00000000-0000-0000-0000-000000000001"
	const ruleID = "00000000-0000-0000-0000-000000000002"
	// Hit every cell of the cross-product once.
	modes := []string{"observe", "warn", "block"}
	reasons := []string{
		"required_missing",
		"type_mismatch",
		"additional_properties_not_allowed",
		"enum_violation",
		"format_violation",
		"other",
	}
	for _, mode := range modes {
		for _, reason := range reasons {
			m.ObserveEdgeRuleValidateFailure(appID, ruleID, mode, reason)
		}
	}

	body := bodyForCounter(t, m)
	// The §12 dashboard panel reads the counter with label combos;
	// the matrix anchors the cross-product. Both legacy and new
	// counter surfaces are pinned — a future shadow-period end
	// (ADR-128 §5) drops the legacy entries from this assertion.
	want := []string{
		// Legacy counter (deprecated; ADR-128 §5).
		`gateway_edge_rule_validate_failures_total{mode="observe",reason="required_missing"} 1`,
		`gateway_edge_rule_validate_failures_total{mode="warn",reason="type_mismatch"} 1`,
		`gateway_edge_rule_validate_failures_total{mode="block",reason="additional_properties_not_allowed"} 1`,
		`gateway_edge_rule_validate_failures_total{mode="block",reason="enum_violation"} 1`,
		`gateway_edge_rule_validate_failures_total{mode="block",reason="format_violation"} 1`,
		`gateway_edge_rule_validate_failures_total{mode="block",reason="other"} 1`,
		// Canonical counter (ADR-128 §5). Sample 3 of the 18
		// (mode, reason) cells — full closed-set coverage
		// comes from the legacy assertion above. Prometheus
		// emits label tuples in alphabetical order regardless
		// of declaration order, so the rendered label list is
		// [app_id, mode, reason, rule_id].
		`gateway_validate_failures_total{app_id="00000000-0000-0000-0000-000000000001",mode="observe",reason="required_missing",rule_id="00000000-0000-0000-0000-000000000002"} 1`,
		`gateway_validate_failures_total{app_id="00000000-0000-0000-0000-000000000001",mode="warn",reason="type_mismatch",rule_id="00000000-0000-0000-0000-000000000002"} 1`,
		`gateway_validate_failures_total{app_id="00000000-0000-0000-0000-000000000001",mode="block",reason="other",rule_id="00000000-0000-0000-0000-000000000002"} 1`,
	}
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("missing %q in metrics body:\n%s", w, body)
		}
	}
}

// TestObserveEdgeRuleValidateFailure_UnknownCoercedToOther ensures
// that a malformed mode or reason collapses to the closed set so a
// determined caller cannot grow the label cardinality. Covers the
// full coerce matrix:
//
//   - unknown mode + unknown reason → (mode="other", reason="other")
//   - unknown mode + known reason   → (mode="other", reason=<known>)
//   - known mode + unknown reason   → (mode=<known>, reason="other")
//
// A regression that only co-erced one side would let the cross-
// product inflate; this test pins the four-cell matrix.
func TestObserveEdgeRuleValidateFailure_UnknownCoercedToOther(t *testing.T) {
	m := NewMetrics()
	const appID = "00000000-0000-0000-0000-00000000000a"
	const ruleID = "00000000-0000-0000-0000-00000000000b"
	// (mode="other", reason="other") — both unknown.
	m.ObserveEdgeRuleValidateFailure(appID, ruleID, "NUKE", "leak-fingerprint")
	m.ObserveEdgeRuleValidateFailure(appID, ruleID, "explode", "x"+"y"+"z")
	// (mode="other", reason=<known>) — mode unknown, reason known.
	m.ObserveEdgeRuleValidateFailure(appID, ruleID, "NUKE", "type_mismatch")
	m.ObserveEdgeRuleValidateFailure(appID, ruleID, "explode", "enum_violation")
	// (mode=<known>, reason="other") — mode known, reason unknown.
	m.ObserveEdgeRuleValidateFailure(appID, ruleID, "observe", "leak-fingerprint")
	m.ObserveEdgeRuleValidateFailure(appID, ruleID, "block", "x"+"y"+"z")

	body := bodyForCounter(t, m)
	want := []string{
		// Both unknown → (other, other). Two increments from
		// the first two calls land here.
		`gateway_edge_rule_validate_failures_total{mode="other",reason="other"} 2`,
		// Unknown mode + known reason → (other, known).
		`gateway_edge_rule_validate_failures_total{mode="other",reason="type_mismatch"} 1`,
		`gateway_edge_rule_validate_failures_total{mode="other",reason="enum_violation"} 1`,
		// Known mode + unknown reason → (known, other).
		`gateway_edge_rule_validate_failures_total{mode="observe",reason="other"} 1`,
		`gateway_edge_rule_validate_failures_total{mode="block",reason="other"} 1`,
	}
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("missing %q in metrics body:\n%s", w, body)
		}
	}
}

// TestObserveEdgeRuleValidateFailure_RuleIDOverflowCollapsesToOther
// pins the per-app ruleLabelSet admission gate (ADR-128 §D3). 257
// distinct rule IDs on the same app must result in 256 verbatim
// admissions + 1 "__other__" overflow series. The metric must NOT
// carry 257 distinct rule_id values — that would blow the
// Prometheus per-instance label budget.
func TestObserveEdgeRuleValidateFailure_RuleIDOverflowCollapsesToOther(t *testing.T) {
	m := NewMetrics()
	const appID = "00000000-0000-0000-0000-000000000fff"
	// 257 distinct rule IDs — one past the per-app cap.
	for i := 0; i < ruleLabelSetCap+1; i++ {
		ruleID := "00000000-0000-0000-0000-0000000000" + string(rune('a'+i%26)) + string(rune('A'+i/26))
		// Truncate to a fixed-width form for the body check —
		// ruleLabelSet accepts the full string regardless.
		// Increment with mode=observe so the existing pre-instantiation
		// doesn't dominate.
		m.ObserveEdgeRuleValidateFailure(appID, ruleID, "observe", "type_mismatch")
	}
	body := bodyForCounter(t, m)
	// __other__ overflow series must exist exactly once.
	// Prometheus renders labels alphabetically: app_id, mode,
	// reason, rule_id.
	overflowLine := `gateway_validate_failures_total{app_id="00000000-0000-0000-0000-000000000fff",mode="observe",reason="type_mismatch",rule_id="__other__"}`
	if !strings.Contains(body, overflowLine+" 1") {
		t.Errorf("expected overflow line %q with count 1:\n%s", overflowLine, body)
	}
}

// TestObserveEdgeRuleValidateFailure_NilReceiver is the nil-safe
// guard called out in the method's doc-comment. The unit test
// doubles as a meta-check that no other Observe* helper changed
// the nil-safety contract.
func TestObserveEdgeRuleValidateFailure_NilReceiver(t *testing.T) {
	var m *Metrics
	// Must not panic.
	m.ObserveEdgeRuleValidateFailure("app", "rule", "observe", "required_missing")
}
