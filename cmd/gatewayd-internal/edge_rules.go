// Edge-rule matcher wiring for gatewayd-internal (ADR-089 / issue #561,
// PR 3). The cache primitive + interfaces live in pkg/gateway/edge_rules.go;
// this file is the production seam where `state.EdgeRule → gateway.EdgeRuleResolved`
// happens, the pg_notify invalidation loop in cmd/gatewayd-internal/backend.go
// calls Reset() on, and the handler calls MatchRoute on.
//
// PR 3 ships `kind=route` only. PR 4-7 widen the same gatewaydEdgeRules
// struct with the rewrite / redirect / headers / cors / ip / jwt matcher
// branches; the cache + invalidation plumbing stays unchanged.

package main

import (
	"context"
	"log/slog"
	"sort"
	"strings"

	"github.com/onebox-faas/faas/pkg/gateway"
	"github.com/onebox-faas/faas/pkg/state"
)

// edgeRuleStore is the slice of state.Store the matcher needs.
// Pinning the interface keeps cmd/gatewayd-internal free of any
// reverse dep on the full state.Store surface so tests can inject
// a tiny fake that returns canned rule slices.
type edgeRuleStore interface {
	MatchEdgeRulesForHost(ctx context.Context, host string) ([]state.EdgeRule, error)
}

// gatewaydEdgeRules is the production gateway.EdgeRuleMatcher impl.
// Per-host cache + state.Store-backed loader + a no-op auditor thin
// wrapper. nil-safe (the gateway handler skips MatchRoute when
// h.edgeRules == nil; this type is never nil because cmd/gatewayd-internal/run.go
// always wires one before the listener accepts).
type gatewaydEdgeRules struct {
	store edgeRuleStore
	cache *gateway.EdgeRuleCache
	log   *slog.Logger
}

// newGatewaydEdgeRules builds the matcher with the standard
// 10,000-entry LRU capacity (pkg/gateway/edgeRuleCacheCap). log
// must be non-nil so loader failures have somewhere to land.
func newGatewaydEdgeRules(store edgeRuleStore, log *slog.Logger) *gatewaydEdgeRules {
	return &gatewaydEdgeRules{
		store: store,
		cache: gateway.NewEdgeRuleCache(gateway.EdgeRuleCacheCap),
		log:   log,
	}
}

// MatchRoute returns the highest-priority `kind=route` rule whose
// host, path, and method match the inbound request, or nil if no
// rule applies. The cache is checked first; a miss falls through
// to MatchEdgeRulesForHost on the store and the resulting slice is
// compiled + Put back into the cache. Compiled rules are sorted
// priority-ASC so pickFirstRouteMatch (pkg/gateway) returns the
// lowest-numbered match — same shape as the existing
// RouteResolver/RouteCache hot path.
//
// Method filtering + path glob matching happen in
// pickFirstRouteMatch (pkg/gateway/edge_rules.go) so the filter
// logic is pinned in pkg/gateway unit tests and can't silently
// drift if this loader is refactored.
//
// Compilation does three things per rule:
//
//  1. Skip rules where Enabled=false (the same gate the apid
//     applies to Enable toggles; disabled rules must NEVER steer
//     traffic).
//  2. Skip rules where Kind != EdgeRuleKindRoute (PR 4-7 widen
//     with their own kinds; this loader only compiles route rules
//     into the subset type the matcher reads).
//  3. Compile MatchMethods into a `map[string]bool` lookup table
//     so the per-request filter is O(1) instead of scanning the
//     slice.
//
// The MatchPath string is passed through verbatim — pickFirstRouteMatch
// runs stdlib path.Match at filter time so any glob the customer
// wrote is honoured (escaping rules are documented in the spec §13.4).
func (g *gatewaydEdgeRules) MatchRoute(ctx context.Context, host, requestPath, method string) *gateway.EdgeRuleResolved {
	if g == nil || g.cache == nil {
		return nil
	}
	rules, hit := g.cache.Get(host)
	if !hit {
		storeRules, err := g.store.MatchEdgeRulesForHost(ctx, host)
		if err != nil {
			if g.log != nil {
				g.log.Warn("edge rule loader failed; treating as miss", "host", host, "err", err)
			}
			return nil
		}
		rules = compileRouteRules(storeRules)
		g.cache.Put(host, rules)
	}
	return gateway.PickFirstRouteMatch(rules, requestPath, method)
}

// Reset drops every cached entry. Called by the pg_notify loop in
// cmd/gatewayd-internal/backend.go on db.NotifyEdgeRuleChanged
// (mirrors PGBackend.FlushRoutes).
func (g *gatewaydEdgeRules) Reset() {
	if g == nil || g.cache == nil {
		return
	}
	g.cache.Reset()
}

// compileRouteRules filters storeRules to the kind=route subset,
// compiles the per-rule filter tables, and sorts priority-ASC so
// pickFirstRouteMatch returns the lowest-numbered match first.
// Empty rules slice in -> empty rules slice out (Put is a no-op
// per pkg/gateway/edge_rules.go:105-108).
func compileRouteRules(storeRules []state.EdgeRule) []gateway.EdgeRuleResolved {
	if len(storeRules) == 0 {
		return nil
	}
	out := make([]gateway.EdgeRuleResolved, 0, len(storeRules))
	for i := range storeRules {
		r := &storeRules[i]
		if !r.Enabled {
			continue
		}
		if r.Kind != state.EdgeRuleKindRoute {
			continue
		}
		if r.Action.Route == nil {
			continue
		}
		var methods map[string]bool
		if len(r.MatchMethods) > 0 {
			methods = make(map[string]bool, len(r.MatchMethods))
			for _, m := range r.MatchMethods {
				methods[strings.ToUpper(m)] = true
			}
		}
		out = append(out, gateway.EdgeRuleResolved{
			ID:            r.ID,
			AccountID:     r.AccountID,
			AppID:         r.AppID,
			Priority:      r.Priority,
			PathGlob:      r.MatchPath,
			Methods:       methods,
			TargetAppSlug: r.Action.Route.TargetAppSlug,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out
}

// gatewaydEdgeRulesAud is the cmd/gatewayd-internal audit thin wrapper
// the handler's edge_rule.route_matched / edge_rule.route_blocked rows
// go through. Reuses the existing *gatewaydAuditor (cmd/gatewayd-internal/audit.go)
// so the rows land in the same events table every other gatewayd-scope
// row uses. The pkg/gateway handler imports `gateway.EdgeRuleAuditor`
// (the narrow interface), not *gatewaydAuditor directly, to keep the
// pkg/gateway ← cmd/gatewayd-internal dep direction one-way.
type gatewaydEdgeRulesAud struct {
	inner *gatewaydAuditor
}

// Emit forwards to the underlying gatewaydAuditor. Nil-safe on the
// receiver so unit tests can pass nil and drop the audit row.
func (a *gatewaydEdgeRulesAud) Emit(ctx context.Context, kind string, subject *string, data map[string]any) {
	if a == nil || a.inner == nil {
		return
	}
	a.inner.Emit(ctx, kind, subject, data)
}

// newGatewaydEdgeRulesAud wraps an existing *gatewaydAuditor. The
// production wiring in cmd/gatewayd-internal/run.go calls this
// next to newGatewaydAuditor.
func newGatewaydEdgeRulesAud(inner *gatewaydAuditor) *gatewaydEdgeRulesAud {
	return &gatewaydEdgeRulesAud{inner: inner}
}

// Compile-time check: gatewaydEdgeRules satisfies gateway.EdgeRuleMatcher.
// Fails to compile if the matcher interface widens in pkg/gateway
// and the impl forgets to add the new method.
var _ gateway.EdgeRuleMatcher = (*gatewaydEdgeRules)(nil)

// Compile-time check: gatewaydEdgeRulesAud satisfies gateway.EdgeRuleAuditor.
var _ gateway.EdgeRuleAuditor = (*gatewaydEdgeRulesAud)(nil)
