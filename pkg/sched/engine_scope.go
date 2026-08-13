// engine_scope.go — PR-B (issue #272) scope plumbing for wake/admit.
//
// Scope semantics (locked with the user, plan §PR-B):
//   - Scope ""  = prod (legacy single-deployment behaviour, every
//     pre-PR-B caller unaffected; the engine treats ScopeFrom(ctx)
//     == "" exactly like the pre-PR-B path).
//   - Scope "pr-{N}" = preview on parent slug.
//
// Why a context key (and not a 4th method parameter on every helper):
//   - resolveApp / loadAPIEnv / admitAndDispatch all already thread
//     the engine-wide *ctx. Adding `scope string` to their public
//     signatures forces every internal call site to carry it; a
//     typed context key keeps the helper signatures byte-identical
//     to pre-PR-B and pins the "empty == legacy" invariant via
//     ScopeFrom's zero-value return.
//
// Wire surface:
//   - gateway calls Engine.Wake(ctx, appID, deploymentID, scope) →
//     Wake wraps the ctx via WithScope before any internal helper
//     reads it.
//   - proto widening: WakeRequest.scope (PR-B proto field, default
//     empty) → grpc handler wraps ctx via WithScope before calling
//     Wake.
//
// Test seam:
//   - MustScope / ScopeFrom are nil-safe and zero-value-safe —
//     tests that pre-date PR-B don't need to know about them.

package sched

import "context"

// scopeCtxKey is the unexported typed key for scope on the wake
// context. Using a struct{} type prevents accidental key collisions
// with other context values (the "typed key" idiom — see Go's
// context docs and ADR-016's "context keys" guidance).
type scopeCtxKey struct{}

// WithScope returns a new context that carries scope for downstream
// Engine helpers to read via ScopeFrom. Empty scope is preserved
// (no special-casing) — the engine's wake paths branch on
// ScopeFrom(ctx) == "" exactly the same way they branch on a
// direct scope == "" argument would have been branched.
//
// Callers wrap ONCE at the engine entry point (Wake / AdmitInstance
// / AdmitInstanceForDeployment) so every helper that reads the
// context sees the same value.
func WithScope(ctx context.Context, scope string) context.Context {
	if scope == "" {
		// Avoid stamping the context for the legacy zero-value
		// case — ScopeFrom then returns "" naturally, and we
		// don't pay the context.Value chain overhead on every
		// helper call in the prod hot path.
		return ctx
	}
	return context.WithValue(ctx, scopeCtxKey{}, scope)
}

// ScopeFrom returns the scope stamped via WithScope on ctx, or ""
// when ctx carries no scope. The zero-value default preserves the
// legacy single-deployment behaviour for every call site that
// doesn't wrap (Wake from cron, meterd, e2e tests, etc.).
func ScopeFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(scopeCtxKey{}).(string)
	return v
}

// MustScope is a test-only helper that returns scope and panics on
// any non-string value (defence-in-depth — context.WithValue with
// a typed key cannot collide, but a future refactor that uses a
// raw string key could). Tests that need to assert a scope was
// threaded correctly can call MustScope(ctx) and compare.
func MustScope(ctx context.Context) string {
	if ctx == nil {
		panic("sched: MustScope: nil ctx")
	}
	v, ok := ctx.Value(scopeCtxKey{}).(string)
	if !ok {
		panic("sched: MustScope: ctx missing scope")
	}
	return v
}
