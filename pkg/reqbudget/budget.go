// Package reqbudget: budget.go — Budget value, ctx carrier, per-hop
// wrap helpers. See doc.go for the package-level contract.
package reqbudget

import (
	"context"
	"time"
)

// Source tags how a Budget came into being. Useful for diagnostics
// (logs) and for picking ceiling defaults in production.
type Source string

const (
	SourceStdlib   Source = "stdlib"   // set by stdlib http.Server config (defensive)
	SourceEdge     Source = "edge"     // set by BudgetMiddleware at the edge
	SourceExplicit Source = "explicit" // set by an explicit per-route rule (kind=budget)
)

// HopMargin is an audit entry recording that a downstream hop
// reserved Cost wall-clock against the parent Remaining. Stacked on
// the Budget, surfaced in logs, and labelled on the
// request_budget_seconds histogram when that hop is the first to fire.
type HopMargin struct {
	Name string        // "db", "grpc", "http", "queue", "stream", "edge", …
	Cost time.Duration // reservation, not measurement
}

// Budget is the immutable value carried in context.Context. Total +
// Started + Ceiling are the three knobs that pin a deadline; the
// derived helpers (Remaining, WithOverhead, WithCeiling) read them.
// Overheads is an append-only audit trail of hop reservations. Now
// is the per-Budget clock — parents carry their clock handle to
// children so a test can fake the clock and still see elapsed time
// advance across the call sequence.
//
// A Budget has value semantics: it is safe to copy by value but the
// audit trail (Overheads) is only meaningful on the instance the
// per-hop helpers returned, not on the parent. Callers should always
// use the fresh Budget returned by WithOverhead / WithCeiling on
// downstream calls.
type Budget struct {
	Total     time.Duration    // originally allotted wall-clock
	Started   time.Time        // wall-clock anchor
	Ceiling   time.Duration    // hard upper bound on Remaining at this hop
	Overheads []HopMargin      // reserved costs to date (audit trail)
	Endpoint  string           // "POST:/payment" — metric + log label
	Route     string           // "forward" | "admin" | "invoke" | "edge"
	Source    Source           // diagnostic tag
	Now       func() time.Time // per-Budget clock; nil → time.Now
}

// DefaultClock is the production wall clock. Tests override
// per-Budget via Budget.Now so the package doesn't need a global.
var DefaultClock = time.Now

// now returns b.Now if set, else the package default. Production code
// leaves b.Now nil so this resolves to time.Now at every call.
func (b Budget) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return DefaultClock()
}

// budgetKey is the unexported context key under which a Budget is
// stored. Unexported so callers can't introspect or collide.
type budgetKey struct{}

// FromContext returns the Budget attached to ctx and true, or the
// zero Budget and false when no Budget is attached. The bool is the
// load-bearing piece: call-sites without a budget (internal goroutines,
// admin paths that pre-date the middleware) take the no-op branch in
// WithOverhead / WithCeiling / WithRemaining.
func FromContext(ctx context.Context) (Budget, bool) {
	b, ok := ctx.Value(budgetKey{}).(Budget)
	return b, ok
}

// NewContext attaches b to parent. It does NOT install a deadline on
// the returned context — that is the responsibility of the caller
// (WithRemaining / WithOverhead / WithCeiling). NewContext exists so
// tests can pin a Budget onto a context without touching deadlines.
func NewContext(parent context.Context, b Budget) context.Context {
	return context.WithValue(parent, budgetKey{}, b)
}

// Remaining is the wall-clock budget left at time `b.now()`. Negative
// remaining clamps to zero (a request that has already overshot its
// budget reports 0, not -3ms). Tests override b.Now for deterministic
// math; production leaves it nil and b.now() resolves to
// DefaultClock (time.Now).
func (b Budget) Remaining(_ time.Time) time.Duration {
	if b.Total <= 0 {
		return 0
	}
	now := b.now()
	elapsed := now.Sub(b.Started)
	if elapsed < 0 {
		elapsed = 0
	}
	r := b.Total - elapsed
	if r < 0 {
		return 0
	}
	return r
}

// derivedChild derives a child context whose deadline is
// min(parentRemaining, ceiling - sumOfOverheadsReserved). Both arguments
// are required; the helper never silently widens. The returned Budget
// has the new total (the capped value), preserves Started and the
// audit trail from parent.
//
// If parentBudget is the zero value (no Budget on ctx), derivedChild
// returns the parent ctx unchanged with a no-op cancel — an identity
// no-op so callers without a budget are unaffected.
func derivedChild(parentBudget Budget, parent context.Context, childTotal, ceiling time.Duration, hopName string, hopCost time.Duration) (context.Context, context.CancelFunc, Budget) {
	if parentBudget.Total == 0 {
		// No budget on ctx — be the parent, no cancellation churn.
		return parent, func() {}, Budget{}
	}
	if childTotal < 0 {
		childTotal = 0
	}
	childCtx, cancel := context.WithTimeout(parent, childTotal)
	childBudget := parentBudget
	childBudget.Total = childTotal
	childBudget.Ceiling = ceiling
	// Per-hop Started: the child's clock anchor is the moment of
	// attach. Remaining math on the child reads
	// childBudget.Remaining = childTotal - (now - childStarted). The
	// parent budget's already-consumed elapsed time is captured in
	// childTotal itself; this keeps child Remaining() honest
	// regardless of how much wall time passed between parent and
	// child attach.
	childBudget.Started = parentBudget.now()
	// Per-Budget clock handle: a child inherits its parent's clock so
	// tests can fake the wall clock end-to-end without touching
	// global state. Production leaves parentBudget.Now nil and the
	// clock handle resolves to DefaultClock at every call.
	childBudget.Now = parentBudget.Now
	if hopName != "" && hopCost > 0 {
		childBudget.Overheads = append(childBudget.Overheads, HopMargin{Name: hopName, Cost: hopCost})
	}
	return childCtx, cancel, childBudget
}

// WithCeiling is the wrap for hops with an absolute upper bound (JWT
// verify 5s, fwdStream 910s, rawStream 24h, dashboard PromQL 3s):
// child deadline = min(parentRemaining, ceiling). The ceiling is an
// absolute "this hop never takes more than X" — it can only tighten
// the parent budget, never loosen it. Returns the wrapped ctx, a
// cancel that MUST be defer'd by the caller, and the new child
// Budget.
//
// When no Budget is on parent, WithCeiling returns the parent ctx
// unchanged with a no-op cancel — call-sites without a budget don't
// change behavior.
func (b Budget) WithCeiling(parent context.Context, ceiling time.Duration) (context.Context, context.CancelFunc, Budget) {
	if b.Total == 0 {
		return parent, func() {}, Budget{}
	}
	remaining := b.Remaining(time.Time{})
	if ceiling < 0 {
		ceiling = 0
	}
	childTotal := remaining
	if ceiling < childTotal {
		childTotal = ceiling
	}
	if childTotal < 0 {
		childTotal = 0
	}
	return derivedChild(b, parent, childTotal, ceiling, "", 0)
}

// WithOverhead is the per-hop workhorse: child deadline =
// min(parentRemaining - cost, parentCeiling - Σ(overheads)). cost is
// a reservation, not a measurement — it ensures hop B starts with
// less declared budget than hop A had even before B's own work
// begins.
//
// hopName goes onto the Budget.Overheads audit trail and is the
// `hop=...` label on the request_budget_seconds histogram when this
// hop is the first to fire.
//
// When no Budget is on parent, WithOverhead returns the parent ctx
// unchanged with a no-op cancel.
func (b Budget) WithOverhead(parent context.Context, hopName string, cost time.Duration) (context.Context, context.CancelFunc, Budget) {
	if b.Total == 0 {
		return parent, func() {}, Budget{}
	}
	remaining := b.Remaining(time.Time{})
	if cost < 0 {
		cost = 0
	}
	childTotal := remaining - cost
	if childTotal < 0 {
		childTotal = 0
	}
	// Cap by the parent's ceiling so a child hop never exceeds the
	// ceiling even if parent remaining was generous. childCeiling is
	// carried on the new Budget for further descendants.
	return derivedChild(b, parent, childTotal, b.Ceiling, hopName, cost)
}

// WithRemaining is the edge setter: install a child ctx whose
// deadline is `total` (or any earlier parent deadline that may be on
// the incoming ctx — stdlib http.Server.WriteTimeout attaches a
// earlier deadline on r.Context()) with hard ceiling `ceiling`. The
// returned Budget is attached to the new ctx.
//
// route + endpoint are stamped onto the Budget for metric labels and
// log output. Pass "" for either to leave them blank.
//
// When no Budget is on parent and total > 0, WithRemaining installs
// one. When total <= 0, returns parent unchanged with a no-op cancel
// — useful for handlers that want to explicitly opt out (e.g., an
// admin long-poll).
func WithRemaining(parent context.Context, total, ceiling time.Duration, route, endpoint string) (context.Context, context.CancelFunc, Budget) {
	if total <= 0 {
		return parent, func() {}, Budget{}
	}
	if ceiling <= 0 || ceiling > total {
		ceiling = total
	}
	// Honor any earlier parent deadline: a stdlib http.Server that
	// attached a 60s ReadTimeout means we must not install a 3s
	// budget that ignores it — the tighter one wins.
	if dl, ok := parent.Deadline(); ok {
		untilParent := time.Until(dl)
		if untilParent > 0 && untilParent < total {
			total = untilParent
		}
	}
	started := DefaultClock()
	ctx, cancel := context.WithTimeout(parent, total)
	b := Budget{
		Total:    total,
		Started:  started,
		Ceiling:  ceiling,
		Route:    route,
		Endpoint: endpoint,
		Source:   SourceEdge,
	}
	return context.WithValue(ctx, budgetKey{}, b), cancel, b
}
