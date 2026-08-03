package state

// EffectiveMinInstances returns the customer-facing per-app cold-wake
// floor: max(legacy column, scaling-policy jsonb). ADR-071 (issue #557)
// §Decision 2 — pre-#557 the two sources could diverge because a bare
// SetAppMinInstances PATCH writes only the legacy column (the
// SetScalingPolicy path is the only one that re-projects the jsonb into
// the column — see pkg/state/pgstore.go keepMinInstancesInSync).
//
// Three readers depend on this single number:
//
//   - pkg/sched/loop.go (reaper floor arithmetic, RUNNING count not
//     below floor when parking idle instances)
//   - pkg/sched/engine.go atMinFloorWithNoSignal (the admitGate
//     short-circuit that fires when concurrency already meets the
//     floor — the request-driven wake path's idempotency check)
//   - pkg/meter/sampler.go (ADR-060 billing, "billed from t=0" —
//     sampler emits a synthetic usage_minutes row per gap slot when
//     live count < floor)
//
// Pre-#557 the reaper read the legacy column and the sampler read the
// jsonb; a customer who configured the floor via the legacy PATCH got a
// warm floor they were never billed for (revenue-affecting). Post-#557
// the helper is the single read-side glue; no migration required because
// divergent rows behave correctly the moment the helper ships (max
// returns the safer direction — billing and enforcement both see what
// the customer actually configured).
//
// A future cleanup migration (ADR-071 §Downstream) will backfill
// divergent rows so the legacy column and the jsonb agree on every app;
// it is not in scope for this PR.
func (a *App) EffectiveMinInstances() int {
	return effectiveMinInstances(a)
}

// effectiveMinInstances is the function form so callers holding an App
// by value (e.g. pkg/sched/engine.go's local `app state.App`) can pass
// `&app` without copying the whole struct. Nil-safe.
func effectiveMinInstances(a *App) int {
	if a == nil {
		return 0
	}
	col := a.MinInstances
	pol := ScalingPolicyOrDefault(a.ScalingPolicy).MinInstances
	if pol > col {
		return pol
	}
	return col
}
