// Edge-rule matcher wiring for gatewayd-internal (ADR-089 / issue #561,
// PR 3 + PR 4). The cache primitive + interfaces live in pkg/gateway/edge_rules.go;
// this file is the production seam where `state.EdgeRule → gateway.EdgeRule*Resolved`
// happens, the pg_notify invalidation loop in cmd/gatewayd-internal/backend.go
// calls Reset() on, and the handler calls Match* on.
//
// PR 3 shipped `kind=route`. PR 4 widens with kind=rewrite, kind=redirect,
// kind=headers — three new Match* methods on the same gatewaydEdgeRules
// struct, three new compile* helpers. The cache is a single per-host
// hostEntry (pkg/gateway/edge_rules.go) holding all four kinds' compiled
// slices; recompilation happens once on a miss for any kind (the SQL
// roundtrip dominates, so paying the compile cost four times is
// irrelevant). The single pg_notify invalidation channel
// (`db.NotifyEdgeRuleChanged`) covers all kinds wholesale.
//
// PR 5-7 widen with kind=cors / kind=jwt / kind=ip on the same shape.

package main

import (
	"context"
	"log/slog"
	"path"
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
// wrapper. nil-safe (the gateway handler skips Match* when
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

// loadHost compiles every kind's slice for host into a fresh
// hostEntry. Shared by all four Match* methods so a cache miss for
// ANY kind recompiles all four kinds in one pass (the SQL roundtrip
// dominates; the four path.Match walks are irrelevant). Returns
// nil + nil error on an empty store response.
//
// parseErrs is the aggregated path-glob parse-error list across all
// four compile* calls — the caller logs each at WARN so an operator
// can diagnose a malformed glob. The malformed rules are dropped
// from their respective compiled slices.
func (g *gatewaydEdgeRules) loadHost(ctx context.Context, host string) (*gateway.HostEntry, error) {
	storeRules, err := g.store.MatchEdgeRulesForHost(ctx, host)
	if err != nil {
		return nil, err
	}
	route, routeErrs := compileRouteRules(storeRules)
	rewrite, rewriteErrs := compileRewriteRules(storeRules)
	redirect, redirectErrs := compileRedirectRules(storeRules)
	headers, headersErrs := compileHeadersRules(storeRules)
	entry := &gateway.HostEntry{
		Route:    route,
		Rewrite:  rewrite,
		Redirect: redirect,
		Headers:  headers,
	}
	parseErrs := append(routeErrs, rewriteErrs...)
	parseErrs = append(parseErrs, redirectErrs...)
	parseErrs = append(parseErrs, headersErrs...)
	if len(parseErrs) > 0 {
		entry.PathGlobErrs = parseErrs
	}
	return entry, nil
}

// MatchRoute returns the highest-priority `kind=route` rule whose
// host, path, and method match the inbound request, or nil if no
// rule applies. The cache is checked first; a miss falls through
// to MatchEdgeRulesForHost on the store and the resulting slice is
// compiled + Put back into the cache. Compiled rules are sorted
// priority-ASC so pickFirstMatch (pkg/gateway) returns the
// lowest-numbered match — same shape as the existing
// RouteResolver/RouteCache hot path.
func (g *gatewaydEdgeRules) MatchRoute(ctx context.Context, host, requestPath, method string) *gateway.EdgeRuleResolved {
	if g == nil || g.cache == nil {
		return nil
	}
	rules, hit := g.cache.Get(host)
	if !hit {
		entry, err := g.loadHost(ctx, host)
		if err != nil {
			if g.log != nil {
				g.log.Warn("edge rule loader failed; treating as miss", "host", host, "err", err)
			}
			return nil
		}
		g.warnPathGlobErrs(host, entry.PathGlobErrs)
		rules = entry.Route
		g.cache.Put(host, entry)
	}
	return gateway.PickFirstRouteMatch(rules, requestPath, method)
}

// MatchRewrite returns the highest-priority `kind=rewrite` rule
// whose host, path, and method match, or nil. Same cache primitive
// as MatchRoute — a miss for rewrite triggers the same loadHost
// pass that also recompiles route / redirect / headers.
func (g *gatewaydEdgeRules) MatchRewrite(ctx context.Context, host, requestPath, method string) *gateway.EdgeRuleRewriteResolved {
	if g == nil || g.cache == nil {
		return nil
	}
	rules, hit := g.cache.GetRewrite(host)
	if !hit {
		entry, err := g.loadHost(ctx, host)
		if err != nil {
			if g.log != nil {
				g.log.Warn("edge rule loader failed; treating as miss", "host", host, "err", err)
			}
			return nil
		}
		g.warnPathGlobErrs(host, entry.PathGlobErrs)
		rules = entry.Rewrite
		g.cache.Put(host, entry)
	}
	return gateway.PickFirstRewriteMatch(rules, requestPath, method)
}

// MatchRedirect returns the highest-priority `kind=redirect` rule
// whose host, path, and method match, or nil. Same cache primitive.
func (g *gatewaydEdgeRules) MatchRedirect(ctx context.Context, host, requestPath, method string) *gateway.EdgeRuleRedirectResolved {
	if g == nil || g.cache == nil {
		return nil
	}
	rules, hit := g.cache.GetRedirect(host)
	if !hit {
		entry, err := g.loadHost(ctx, host)
		if err != nil {
			if g.log != nil {
				g.log.Warn("edge rule loader failed; treating as miss", "host", host, "err", err)
			}
			return nil
		}
		g.warnPathGlobErrs(host, entry.PathGlobErrs)
		rules = entry.Redirect
		g.cache.Put(host, entry)
	}
	return gateway.PickFirstRedirectMatch(rules, requestPath, method)
}

// MatchHeaders returns the highest-priority `kind=headers` rule
// whose host, path, and method match, or nil. Same cache primitive.
func (g *gatewaydEdgeRules) MatchHeaders(ctx context.Context, host, requestPath, method string) *gateway.EdgeRuleHeadersResolved {
	if g == nil || g.cache == nil {
		return nil
	}
	rules, hit := g.cache.GetHeaders(host)
	if !hit {
		entry, err := g.loadHost(ctx, host)
		if err != nil {
			if g.log != nil {
				g.log.Warn("edge rule loader failed; treating as miss", "host", host, "err", err)
			}
			return nil
		}
		g.warnPathGlobErrs(host, entry.PathGlobErrs)
		rules = entry.Headers
		g.cache.Put(host, entry)
	}
	return gateway.PickFirstHeadersMatch(rules, requestPath, method)
}

// Reset drops every cached entry. Called by the pg_notify loop in
// cmd/gatewayd-internal/backend.go on db.NotifyEdgeRuleChanged
// (mirrors PGBackend.FlushRoutes). Wholesale flush because the
// cache is per-host keyed and the pg_notify payload doesn't carry
// match_host (same posture as PR 3).
func (g *gatewaydEdgeRules) Reset() {
	if g == nil || g.cache == nil {
		return
	}
	g.cache.Reset()
}

// warnPathGlobErrs logs every path-glob parse error the loader
// returned, at WARN so an operator can diagnose a malformed glob.
// Errors are not surfaced to the customer (they see a clean 404).
func (g *gatewaydEdgeRules) warnPathGlobErrs(host string, errs []gateway.PathGlobError) {
	if g.log == nil || len(errs) == 0 {
		return
	}
	for _, pe := range errs {
		g.log.Warn("edge rule path glob parse error; rule dropped",
			"host", host, "rule_id", pe.RuleID, "glob", pe.Glob, "err", pe.Err)
	}
}

// compileRouteRules filters storeRules to the kind=route subset,
// compiles the per-rule filter tables, validates any path globs
// (review fix R3), and sorts priority-ASC so PickFirstRouteMatch
// returns the lowest-numbered match first. Empty rules slice in ->
// empty rules slice out (Put is a no-op per pkg/gateway/edge_rules.go).
//
// Path-glob validation: each rule's MatchPath is run through
// stdlib path.Match(glob, "") which returns an error for a
// malformed pattern (unmatched bracket, etc.) without depending
// on the specific request path. A malformed rule is dropped
// from the compiled slice AND the caller receives a parallel
// slice of (rule_id, glob, err) tuples via pathGlobErrors so
// an operator can diagnose the typo — a silent 404 (the prior
// behaviour) made the typo invisible.
//
// Returns (rules, []PathGlobError). Rules is sorted priority-ASC;
// PathGlobErrors is in input order. Nil PathGlobErrors when every
// glob parses cleanly.
func compileRouteRules(storeRules []state.EdgeRule) ([]gateway.EdgeRuleResolved, []gateway.PathGlobError) {
	if len(storeRules) == 0 {
		return nil, nil
	}
	out := make([]gateway.EdgeRuleResolved, 0, len(storeRules))
	var parseErrs []gateway.PathGlobError
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
		if errs := validatePathGlob(r.ID, r.MatchPath); errs != nil {
			parseErrs = append(parseErrs, errs...)
			continue
		}
		out = append(out, gateway.EdgeRuleResolved{
			ID:            r.ID,
			AccountID:     r.AccountID,
			AppID:         r.AppID,
			Priority:      r.Priority,
			PathGlob:      r.MatchPath,
			Methods:       buildMethodsMap(r.MatchMethods),
			TargetAppSlug: r.Action.Route.TargetAppSlug,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out, parseErrs
}

// compileRewriteRules mirrors compileRouteRules for kind=rewrite.
// Same filters (Enabled, Kind, action-nil), same path-glob validation,
// same priority-ASC sort. The compiled slice carries the From/To
// pair the handler's matchAndApplyRewrite uses to mutate r.URL.Path.
func compileRewriteRules(storeRules []state.EdgeRule) ([]gateway.EdgeRuleRewriteResolved, []gateway.PathGlobError) {
	if len(storeRules) == 0 {
		return nil, nil
	}
	out := make([]gateway.EdgeRuleRewriteResolved, 0, len(storeRules))
	var parseErrs []gateway.PathGlobError
	for i := range storeRules {
		r := &storeRules[i]
		if !r.Enabled {
			continue
		}
		if r.Kind != state.EdgeRuleKindRewrite {
			continue
		}
		if r.Action.Rewrite == nil {
			continue
		}
		if errs := validatePathGlob(r.ID, r.MatchPath); errs != nil {
			parseErrs = append(parseErrs, errs...)
			continue
		}
		out = append(out, gateway.EdgeRuleRewriteResolved{
			ID:        r.ID,
			AccountID: r.AccountID,
			AppID:     r.AppID,
			Priority:  r.Priority,
			PathGlob:  r.MatchPath,
			Methods:   buildMethodsMap(r.MatchMethods),
			From:      r.Action.Rewrite.From,
			To:        r.Action.Rewrite.To,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out, parseErrs
}

// compileRedirectRules mirrors compileRouteRules for kind=redirect.
// StatusCode defaults to 302 (pkg/api/dto.go Validate already
// constrains to {301,302,307,308}, so any non-zero value reaching
// here is valid).
func compileRedirectRules(storeRules []state.EdgeRule) ([]gateway.EdgeRuleRedirectResolved, []gateway.PathGlobError) {
	if len(storeRules) == 0 {
		return nil, nil
	}
	out := make([]gateway.EdgeRuleRedirectResolved, 0, len(storeRules))
	var parseErrs []gateway.PathGlobError
	for i := range storeRules {
		r := &storeRules[i]
		if !r.Enabled {
			continue
		}
		if r.Kind != state.EdgeRuleKindRedirect {
			continue
		}
		if r.Action.Redirect == nil {
			continue
		}
		if errs := validatePathGlob(r.ID, r.MatchPath); errs != nil {
			parseErrs = append(parseErrs, errs...)
			continue
		}
		status := r.Action.Redirect.StatusCode
		if status == 0 {
			status = 302 // http.StatusFound — pkg/api Validate already enforced the {301,302,307,308} set
		}
		out = append(out, gateway.EdgeRuleRedirectResolved{
			ID:         r.ID,
			AccountID:  r.AccountID,
			AppID:      r.AppID,
			Priority:   r.Priority,
			PathGlob:   r.MatchPath,
			Methods:    buildMethodsMap(r.MatchMethods),
			StatusCode: status,
			To:         r.Action.Redirect.To,
			Headers:    r.Action.Redirect.Headers,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out, parseErrs
}

// compileHeadersRules mirrors compileRouteRules for kind=headers.
// The compiled slice preserves the customer's declared op order
// (Cloudflare "first wins" semantics for `set`); both
// RequestHeaders and ResponseHeaders arrays are copied verbatim.
func compileHeadersRules(storeRules []state.EdgeRule) ([]gateway.EdgeRuleHeadersResolved, []gateway.PathGlobError) {
	if len(storeRules) == 0 {
		return nil, nil
	}
	out := make([]gateway.EdgeRuleHeadersResolved, 0, len(storeRules))
	var parseErrs []gateway.PathGlobError
	for i := range storeRules {
		r := &storeRules[i]
		if !r.Enabled {
			continue
		}
		if r.Kind != state.EdgeRuleKindHeaders {
			continue
		}
		if r.Action.Headers == nil {
			continue
		}
		if errs := validatePathGlob(r.ID, r.MatchPath); errs != nil {
			parseErrs = append(parseErrs, errs...)
			continue
		}
		out = append(out, gateway.EdgeRuleHeadersResolved{
			ID:              r.ID,
			AccountID:       r.AccountID,
			AppID:           r.AppID,
			Priority:        r.Priority,
			PathGlob:        r.MatchPath,
			Methods:         buildMethodsMap(r.MatchMethods),
			RequestHeaders:  convertHeaderOps(r.Action.Headers.RequestHeaders),
			ResponseHeaders: convertHeaderOps(r.Action.Headers.ResponseHeaders),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out, parseErrs
}

// validatePathGlob runs stdlib path.Match(glob, "") to detect a
// malformed pattern (unmatched bracket, etc.) without depending
// on the specific request path. Returns a 0/1-length PathGlobError
// slice — nil when the glob is empty ("") or "*" (match-all
// sentinels that stdlib rejects on the empty-input probe) or
// parses cleanly. PR 3 review-fix R3 introduced this so a typo
// is operator-visible instead of silently 404.
func validatePathGlob(ruleID, glob string) []gateway.PathGlobError {
	if glob == "" || glob == "*" {
		return nil
	}
	if _, err := path.Match(glob, ""); err != nil {
		return []gateway.PathGlobError{{RuleID: ruleID, Glob: glob, Err: err}}
	}
	return nil
}

// buildMethodsMap upper-folds a MatchMethods slice into the
// map[string]bool the per-request filter consults. nil in -> nil out
// (the filter's "nil = any method" short-circuit handles it).
func buildMethodsMap(methods []string) map[string]bool {
	if len(methods) == 0 {
		return nil
	}
	m := make(map[string]bool, len(methods))
	for _, meth := range methods {
		m[strings.ToUpper(meth)] = true
	}
	return m
}

// convertHeaderOps copies a []state.EdgeRuleHeaderOp into the
// gateway-side subset type. Same shape; the cmd-side copies
// instead of importing state from pkg/gateway (pkg/gateway keeps
// zero-dep-on-pkg/state).
func convertHeaderOps(in []state.EdgeRuleHeaderOp) []gateway.EdgeRuleHeaderOp {
	if len(in) == 0 {
		return nil
	}
	out := make([]gateway.EdgeRuleHeaderOp, len(in))
	for i, op := range in {
		out[i] = gateway.EdgeRuleHeaderOp{Name: op.Name, Value: op.Value, Action: op.Action}
	}
	return out
}

// gatewaydEdgeRulesAud is the cmd/gatewayd-internal audit thin wrapper
// the handler's edge_rule.*_matched / edge_rule.*_blocked rows go
// through. Reuses the existing *gatewaydAuditor
// (cmd/gatewayd-internal/audit.go) so the rows land in the same events
// table every other gatewayd-scope row uses. The pkg/gateway handler
// imports `gateway.EdgeRuleAuditor` (the narrow interface), not
// *gatewaydAuditor directly, to keep the pkg/gateway ← cmd/gatewayd-internal
// dep direction one-way.
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
