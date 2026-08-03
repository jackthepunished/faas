package state

// EffectiveMinInstances returns the customer-facing per-deployment
// cold-wake floor. The single source of truth for the three readers
// that consume it (schedd's floor trigger, the reaper, meterd's
// sampler). ADR-072 (issue #557 closure).
//
// Inheritance rule (ADR-072 §Decision 2): the effective per-instance
// floor is `max(app.EffectiveMinInstances(), deployment.EffectiveMinInstances())`.
// The deployment-level column is the explicit override; the parent
// app's floor is the lower bound. A deployment with `min_instances=0`
// (the post-migration default) inherits fully from its parent app.
//
// Schema posture: `deployments.min_instances` lands in this PR (the
// column is the only per-deployment knob). There is no
// `deployments.scaling_policy` jsonb — the deployment-level
// min_instances is set via the PATCH route at
// `PATCH /v1/deployments/{id}` and validated against the *parent
// app's* plan ceiling (the per-plan cap is per-account, not per-row;
// pinning it on the deployment would double the cap and let a
// customer's Scale plan effective floor reach 20 instead of 10).
//
// Why not also project into a jsonb like `apps.scaling_policy`:
// deployment rows don't carry a per-deployment policy shape today;
// the override columns (OverrideEntrypoint / Cmd / Env / Port /
// Healthcheck / Sidecars, issue #460 / ADR-053) are deliberately
// flat because the deployment is a single config snapshot, not a
// scaling-policy target. Adding a jsonb column for one integer would
// widen the surface for no consumer benefit.
func (d *Deployment) EffectiveMinInstances() int {
	return effectiveDeploymentMinInstances(d)
}

// effectiveDeploymentMinInstances is the function form so callers
// holding a Deployment by value can pass `&d` without copying the
// struct. Nil-safe.
func effectiveDeploymentMinInstances(d *Deployment) int {
	if d == nil {
		return 0
	}
	if d.MinInstances < 0 {
		return 0
	}
	return d.MinInstances
}