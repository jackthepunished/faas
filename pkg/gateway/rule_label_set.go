// Bounded admission for the per-app rule_id label on the
// gateway_validate_failures_total{app_id, rule_id, mode, reason}
// counter (issue #975 #3 / ADR-128 §D3).
//
// The (app_id, rule_id) pair is unbounded — a customer can have
// N apps each with M rules. Without admission, a noisy tenant
// could exhaust the Prometheus per-instance label budget. Per-app
// cap of 256 distinct real rule IDs; overflow collapses to the
// reserved "__other__" bucket.
//
// This is the in-package mirror of pkg/wire/rule_label_set.go
// (the cross-daemon contract) and pkg/gateway/account_label_set.go
// / pkg/gateway/hostname_label_set.go / pkg/gateway/route_label_set.go
// (the existing per-package mirrors). pkg/gateway cannot import
// pkg/wire (cycle, documented at cmd/gatewayd-internal/topn.go:14-21),
// so each label-set family has its own copy with the same
// contract.
//
// Behavioural contract:
//
//   - reserved labels ("", "__other__") are pre-admitted at
//     construction without consuming capacity;
//   - empty ruleID normalises to "__other__" (the runtime
//     always supplies one — empty is the pathological case);
//   - real rule IDs per app are admitted up to
//     `cap - reservedCount`;
//   - overflow collapses to "__other__" without ever inserting
//     into the map (so the map never resizes past cap);
//   - the per-app set is non-evicting — daemon restart is the
//     only path that resets it (an evicting LRU would let
//     evicted rule IDs re-admit later and grow the Prometheus
//     TSDB series set unbounded over the daemon's lifetime).
//
// The Prometheus increment happens at the call site AFTER admit()
// returns so it is outside the critical section.
package gateway

import "sync"

// ruleLabelSetCap is the per-app cap for distinct rule IDs
// admitted to gateway_validate_failures_total{app_id, rule_id,
// mode, reason}. Sized for the realistic 95th-percentile
// customer (most apps have <50 rules; apps with >256 rules are
// unusual but possible). Worst-case per-app cardinality is
// (256 rules + 1 __other__) × 3 modes × 6 reasons = 4638 series
// per app, well under Prometheus' per-instance label budget.
const ruleLabelSetCap = 256

// otherRuleLabel is the overflow bucket literal
// ("__other__"). Mirrors pkg/wire.otherRuleLabel and
// pkg/wire.otherBoxLabel.
const otherRuleLabel = "__other__"

// ruleLabelSet is the per-app admission set backing the
// (app_id, rule_id) label pair. Outer map keyed by app_id;
// inner map holds distinct rule IDs admitted for that app.
// Per-app sets are lazily initialized on the first admit
// call for a given app.
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
// Empty ruleID collapses to otherRuleLabel (pathological — the
// runtime always supplies one); (any appID, otherRuleLabel) is
// always admitted without consuming capacity. Real
// (appID, ruleID) pairs consume capacity until the per-app cap
// trips; further distinct rule IDs collapse to otherRuleLabel
// without ever consuming capacity.
//
// Concurrency: holds mu across the lookup+insert. The hot path
// is the "already admitted" lookup, which is O(1) and never
// inserts. Prometheus counter increments happen at the call
// site AFTER admit returns, so they are outside the critical
// section.
//
// Pointer receiver: the type contains a sync.Mutex, so copying
// the value would duplicate the lock (govet copylocks).
// ruleLabelSet is constructed once per Metrics in NewMetrics
// and held as a pointer field.
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
	// (pkg/wire/metrics.go:5953-5957).
	const reservedCount = 1
	realAdmitted := len(set) - reservedCount
	if realAdmitted >= ruleLabelSetCap {
		return otherRuleLabel
	}
	set[ruleID] = struct{}{}
	return ruleID
}
