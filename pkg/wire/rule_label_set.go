package wire

import "sync"

// ADR-128 §D3 — ruleLabelSet is the per-app admission gate for
// the (app_id, rule_id) label pair on
// gateway_validate_failures_total{mode, reason}. The pair is
// unbounded — a customer can have N apps each with M rules —
// so without an admission gate, a single noisy tenant could
// exhaust the Prometheus per-instance label budget.
//
// The shape mirrors boxLabelSet (pkg/wire/metrics.go:5888) and
// accountLabelSet (pkg/wire/metrics.go:5677): plain map + mutex,
// fixed capacity, non-evicting. The outer map is keyed by
// app_id; the inner map holds distinct rule IDs admitted for
// that app. Per-app sets are lazily initialized on the first
// admit call.
//
// Reserved value otherRuleLabel ("__other__") is admitted at
// construction without consuming capacity and is always
// re-admitted on collision-free lookups. Real rule identifiers
// consume capacity once and are never evicted in process — the
// daemon restart is the only path that resets the set.
//
// Sized for the realistic 95th-percentile customer (most apps
// have <50 rules; apps with >256 rules are unusual but
// possible). Worst-case per-app cardinality is
// (ruleLabelSetCap + 1 __other__) × 3 modes × 6 reasons = 4638
// series per app, well under Prometheus' per-instance label
// budget.

// ruleLabelSetCap is the per-app cap for distinct rule IDs
// admitted to the gateway_validate_failures_total{app_id,
// rule_id, mode, reason} metric. See ADR-128 §D3 for sizing.
const ruleLabelSetCap = 256

// otherRuleLabel is the overflow bucket for rule IDs that
// crossed the per-app admission cap. Operators see
// rule_id="__other__" in the Prometheus output and must check
// the gatewayd slog for the original rule id — the metric
// label is intentionally lossy (same contract as
// otherBoxLabel at metrics.go:5660 and otherAccountLabel at
// metrics.go:5619).
const otherRuleLabel = "__other__"

type ruleLabelSet struct {
	mu     sync.Mutex
	perApp map[string]map[string]struct{}
}

// newRuleLabelSet constructs the per-app admission set. The
// outer map is allocated empty and lazily populated on the
// first admit call for a given appID — apps with no rules
// don't consume any map entries.
func newRuleLabelSet() *ruleLabelSet {
	return &ruleLabelSet{perApp: make(map[string]map[string]struct{})}
}

// admit resolves a (appID, ruleID) pair to its label value.
// Empty ruleID collapses to otherRuleLabel (pathological —
// the runtime always supplies one); the empty pair (empty
// appID, any ruleID) also collapses so a daemon startup race
// can't accidentally emit a label with an empty app_id
// component. Real (appID, ruleID) pairs consume capacity
// until the per-app cap trips; further distinct rule IDs
// collapse to otherRuleLabel without ever consuming capacity.
//
// Concurrency: holds mu across the lookup+insert. The hot
// path is the "already admitted" lookup, which is O(1) and
// never inserts. Prometheus counter increments happen at the
// call site AFTER admit returns, so they are outside the
// critical section.
//
// Pointer receiver: the type contains a sync.Mutex, so
// copying the value would duplicate the lock (govet copylocks).
// ruleLabelSet is constructed once per OpsMetrics in
// NewOpsMetrics and held as a pointer field.
func (s *ruleLabelSet) admit(appID, ruleID string) string {
	if ruleID == "" {
		return otherRuleLabel
	}
	if ruleID == otherRuleLabel {
		return ruleID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	set, ok := s.perApp[appID]
	if !ok {
		// Pre-admit the reserved overflow bucket so admit
		// doesn't need a special branch for it on the hot
		// path — the lookup short-circuits through the same
		// map. Capacity is sized at the per-app cap; the
		// reserved entry counts as one map slot, so the
		// real-id check is "len - 1" against ruleLabelSetCap.
		set = make(map[string]struct{}, ruleLabelSetCap)
		set[otherRuleLabel] = struct{}{}
		s.perApp[appID] = set
	}
	if _, ok := set[ruleID]; ok {
		return ruleID
	}
	// Reserved labels (otherRuleLabel) are pre-admitted at
	// construction and consume map entries but NOT user-facing
	// capacity. The user-facing cap of ruleLabelSetCap distinct
	// REAL rule identifiers must hold. The check is therefore
	// "real rules admitted = (len - reserved) >= cap", not
	// "len >= cap" — same bug-fix shape as boxLabelSet.admit
	// (metrics.go:5953-5957) and ipLabelSet.admit.
	const reservedCount = 1
	realAdmitted := len(set) - reservedCount
	if realAdmitted >= ruleLabelSetCap {
		return otherRuleLabel
	}
	set[ruleID] = struct{}{}
	return ruleID
}
