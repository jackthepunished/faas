package deploydiff

import (
	"sort"

	"github.com/onebox-faas/faas/pkg/api"
)

// Compute is the engine entry point. Walks every field in
// [Pending] against the [Baseline], emits [Change] / [Break] rows.
// Pure function — no I/O, no state. The quota gate is a separate
// pass driven by [QuotaConfig] (see quota.go).
//
// Order matters for the renderer's stability:
//  1. App-level scalars (memory, concurrency, …)
//  2. Per-scope env vars (sorted by scope then key)
//  3. Crons (sorted by schedule then path)
//  4. Edge rules (sorted by priority then match-path)
//  5. Deployment-level (image, handler, manifest)
//  6. Schema-break (text-only in PR-0; structural OpenAPI diff is PR-2)
//
// Within each section, changes come before breaks so a customer
// reading the table sees "what would change" before "what would
// fail".
func Compute(slug string, baseline Baseline, pending Pending) Diff {
	plan := inferPlan(baseline.App)
	out := Diff{
		Slug:    slug,
		Changes: []Change{},
		Breaks:  []Break{},
		Plan:    plan,
	}

	// 1. App-level scalars.
	diffAppConfig(&out, baseline.App, pending.AppConfig)

	// 2. Per-scope env vars.
	diffEnvByScope(&out, baseline.EnvByScope, pending.EnvByScope)

	// 3. Crons.
	diffCrons(&out, baseline.Crons, pending.Crons)

	// 4. Edge rules.
	diffEdgeRules(&out, baseline.EdgeRules, pending.EdgeRules)

	// 5. Deployment-level (immutable changes → Break, not Change).
	diffDeployment(&out, baseline.LatestDeployment, pending)

	// 6. Schema-break signal (PR-0: text-only).
	detectSchemaBreak(&out, baseline.LatestDeployment, pending)

	return out
}

// inferPlan reads the plan from the baseline's App.Slug-free view
// (the AppResponse has no Plan field; the apid handler passes it
// separately in PR-1 via [Pending]). PR-0 leaves Plan empty until
// the apid handler wires it through.
func inferPlan(app *api.AppResponse) Plan {
	if app == nil {
		return ""
	}
	return ""
}

// diffAppConfig is the per-scalar pointer-aware comparison. Each
// field: nil Pending → no diff; non-nil differing from baseline →
// Change{Field, Before, After}; non-nil equal to baseline → no diff.
func diffAppConfig(out *Diff, base *api.AppResponse, p AppConfigPatch) {
	if base == nil {
		// Fresh app: every non-nil Pending field is a new-value
		// Change. We still emit them so the customer sees what
		// the create would stamp.
		if p.RAMMB != nil {
			out.Changes = append(out.Changes, Change{
				Field: "memory", Kind: ChangeAdd,
				After: AsAny(*p.RAMMB),
			})
		}
		if p.MaxConcurrency != nil {
			out.Changes = append(out.Changes, Change{
				Field: "concurrency", Kind: ChangeAdd,
				After: AsAny(*p.MaxConcurrency),
			})
		}
		if p.MinInstances != nil {
			out.Changes = append(out.Changes, Change{
				Field: "min_instances", Kind: ChangeAdd,
				After: AsAny(*p.MinInstances),
			})
		}
		if p.IdleTimeoutS != nil {
			out.Changes = append(out.Changes, Change{
				Field: "idle_timeout_s", Kind: ChangeAdd,
				After: AsAny(*p.IdleTimeoutS),
			})
		}
		if p.StreamingEnabled != nil {
			out.Changes = append(out.Changes, Change{
				Field: "streaming_enabled", Kind: ChangeAdd,
				After: AsAny(*p.StreamingEnabled),
			})
		}
		if p.WebSocketEnabled != nil {
			out.Changes = append(out.Changes, Change{
				Field: "websocket_enabled", Kind: ChangeAdd,
				After: AsAny(*p.WebSocketEnabled),
			})
		}
		if p.RequireAuthn != nil {
			out.Changes = append(out.Changes, Change{
				Field: "require_authn", Kind: ChangeAdd,
				After: AsAny(*p.RequireAuthn),
			})
		}
		if p.WarmSnapshotEnabled != nil {
			out.Changes = append(out.Changes, Change{
				Field: "warm_snapshot_enabled", Kind: ChangeAdd,
				After: AsAny(*p.WarmSnapshotEnabled),
			})
		}
		if p.RequireSigned != nil {
			out.Changes = append(out.Changes, Change{
				Field: "require_signed", Kind: ChangeAdd,
				After: AsAny(*p.RequireSigned),
			})
		}
		if p.EvictionPriority != nil {
			out.Changes = append(out.Changes, Change{
				Field: "eviction_priority", Kind: ChangeAdd,
				After: AsAny(*p.EvictionPriority),
			})
		}
		if p.AutoscaleTargetRPS != nil {
			out.Changes = append(out.Changes, Change{
				Field: "autoscale_target_rps", Kind: ChangeAdd,
				After: AsAny(*p.AutoscaleTargetRPS),
			})
		}
		if p.AutoscaleTargetCP != nil {
			out.Changes = append(out.Changes, Change{
				Field: "autoscale_target_cpu_pct", Kind: ChangeAdd,
				After: AsAny(*p.AutoscaleTargetCP),
			})
		}
		if p.EgressAllowlist != nil {
			out.Changes = append(out.Changes, Change{
				Field: "egress_allowlist", Kind: ChangeAdd,
				After: AsAny(*p.EgressAllowlist),
			})
		}
		return
	}

	// Existing app: pointer-aware diff. nil = "don't touch";
	// non-nil differing = Change. Equal values are silently dropped
	// to keep the table uncluttered.
	if p.RAMMB != nil && *p.RAMMB != base.RAMMB {
		out.Changes = append(out.Changes, Change{
			Field: "memory", Kind: ChangeModify,
			Before: AsAny(base.RAMMB), After: AsAny(*p.RAMMB),
		})
	}
	if p.MaxConcurrency != nil && *p.MaxConcurrency != base.MaxConcurrency {
		out.Changes = append(out.Changes, Change{
			Field: "concurrency", Kind: ChangeModify,
			Before: AsAny(base.MaxConcurrency), After: AsAny(*p.MaxConcurrency),
		})
	}
	if p.MinInstances != nil && *p.MinInstances != base.MinInstances {
		out.Changes = append(out.Changes, Change{
			Field: "min_instances", Kind: ChangeModify,
			Before: AsAny(base.MinInstances), After: AsAny(*p.MinInstances),
		})
	}
	if p.IdleTimeoutS != nil && *p.IdleTimeoutS != base.IdleTimeoutS {
		out.Changes = append(out.Changes, Change{
			Field: "idle_timeout_s", Kind: ChangeModify,
			Before: AsAny(base.IdleTimeoutS), After: AsAny(*p.IdleTimeoutS),
		})
	}
	if p.StreamingEnabled != nil && *p.StreamingEnabled != base.StreamingEnabled {
		out.Changes = append(out.Changes, Change{
			Field: "streaming_enabled", Kind: ChangeModify,
			Before: AsAny(base.StreamingEnabled), After: AsAny(*p.StreamingEnabled),
		})
	}
	if p.WebSocketEnabled != nil && *p.WebSocketEnabled != base.WebSocketEnabled {
		out.Changes = append(out.Changes, Change{
			Field: "websocket_enabled", Kind: ChangeModify,
			Before: AsAny(base.WebSocketEnabled), After: AsAny(*p.WebSocketEnabled),
		})
	}
	if p.RequireAuthn != nil && *p.RequireAuthn != base.RequireAuthn {
		out.Changes = append(out.Changes, Change{
			Field: "require_authn", Kind: ChangeModify,
			Before: AsAny(base.RequireAuthn), After: AsAny(*p.RequireAuthn),
		})
	}
	if p.WarmSnapshotEnabled != nil && *p.WarmSnapshotEnabled != base.WarmSnapshotEnabled {
		out.Changes = append(out.Changes, Change{
			Field: "warm_snapshot_enabled", Kind: ChangeModify,
			Before: AsAny(base.WarmSnapshotEnabled), After: AsAny(*p.WarmSnapshotEnabled),
		})
	}
	if p.RequireSigned != nil && *p.RequireSigned != base.RequireSigned {
		out.Changes = append(out.Changes, Change{
			Field: "require_signed", Kind: ChangeModify,
			Before: AsAny(base.RequireSigned), After: AsAny(*p.RequireSigned),
		})
	}
	if p.EvictionPriority != nil && *p.EvictionPriority != base.EvictionPriority {
		out.Changes = append(out.Changes, Change{
			Field: "eviction_priority", Kind: ChangeModify,
			Before: AsAny(base.EvictionPriority), After: AsAny(*p.EvictionPriority),
		})
	}
	if p.AutoscaleTargetRPS != nil && *p.AutoscaleTargetRPS != base.AutoscaleTargetRPS {
		out.Changes = append(out.Changes, Change{
			Field: "autoscale_target_rps", Kind: ChangeModify,
			Before: AsAny(base.AutoscaleTargetRPS), After: AsAny(*p.AutoscaleTargetRPS),
		})
	}
	if p.AutoscaleTargetCP != nil && *p.AutoscaleTargetCP != base.AutoscaleTargetCPUPct {
		out.Changes = append(out.Changes, Change{
			Field: "autoscale_target_cpu_pct", Kind: ChangeModify,
			Before: AsAny(base.AutoscaleTargetCPUPct), After: AsAny(*p.AutoscaleTargetCP),
		})
	}
	if p.EgressAllowlist != nil && !stringSliceEqual(*p.EgressAllowlist, base.EgressAllowlist) {
		out.Changes = append(out.Changes, Change{
			Field: "egress_allowlist", Kind: ChangeModify,
			Before: AsAny(base.EgressAllowlist), After: AsAny(*p.EgressAllowlist),
		})
	}
}

// diffEnvByScope walks Pending.EnvByScope vs Baseline.EnvByScope.
// Per-scope full-replacement semantics — adding a key to a scope
// is an "add", removing it is a "remove", changing the value (which
// the wire only carries by name here) is a "modify" of the key set.
func diffEnvByScope(out *Diff, base map[string][]string, pending map[string][]PendingEnv) {
	if pending == nil {
		return // explicit "no env change" — nil sentinel
	}
	// Union of scope keys. Sorted for stable output.
	keys := make([]string, 0, len(base)+len(pending))
	for k := range base {
		keys = append(keys, k)
	}
	for k := range pending {
		// dedup
		found := false
		for _, x := range keys {
			if x == k {
				found = true
				break
			}
		}
		if !found {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, scope := range keys {
		baseKeys := base[scope]
		pendRows := pending[scope]
		baseSet := keySet(baseKeys)
		pendSet := keySetFromPending(pendRows)
		for k := range pendSet {
			if _, ok := baseSet[k]; !ok {
				field := "environment." + scope + "." + k
				out.Changes = append(out.Changes, Change{
					Field: field, Kind: ChangeAdd,
					After: AsAny(k + " (added to scope " + scope + ")"),
				})
			}
		}
		for k := range baseSet {
			if _, ok := pendSet[k]; !ok {
				field := "environment." + scope + "." + k
				out.Changes = append(out.Changes, Change{
					Field: field, Kind: ChangeRemove,
					Before: AsAny(k + " (present in scope " + scope + ")"),
				})
			}
		}
	}
}

// keySetFromPending turns a []PendingEnv into a key-set for fast
// lookup. Used by diffEnvByScope's add/remove walk.
func keySetFromPending(rows []PendingEnv) map[string]struct{} {
	out := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		out[r.Key] = struct{}{}
	}
	return out
}

// diffCrons compares the per-app cron list. Unique key is
// (schedule, path) per migration 00210; enabled is a per-row
// property.
func diffCrons(out *Diff, base []api.CronResponse, pending []api.CreateCronRequest) {
	if pending == nil {
		return
	}
	// Index base by (schedule, path).
	type cronKey struct{ schedule, path string }
	baseByKey := map[cronKey]api.CronResponse{}
	for _, c := range base {
		baseByKey[cronKey{c.Schedule, c.Path}] = c
	}
	// Index pending by same key.
	pendByKey := map[cronKey]api.CreateCronRequest{}
	for _, c := range pending {
		pendByKey[cronKey{c.Schedule, c.Path}] = c
	}

	// Sorted keys for stable output.
	allKeys := make([]cronKey, 0, len(baseByKey)+len(pendByKey))
	for k := range baseByKey {
		allKeys = append(allKeys, k)
	}
	for k := range pendByKey {
		if _, ok := baseByKey[k]; !ok {
			allKeys = append(allKeys, k)
		}
	}
	sort.Slice(allKeys, func(i, j int) bool {
		if allKeys[i].schedule != allKeys[j].schedule {
			return allKeys[i].schedule < allKeys[j].schedule
		}
		return allKeys[i].path < allKeys[j].path
	})

	for _, k := range allKeys {
		field := "cron[" + k.schedule + " " + k.path + "]"
		b, bOK := baseByKey[k]
		p, pOK := pendByKey[k]
		switch {
		case bOK && !pOK:
			out.Changes = append(out.Changes, Change{
				Field: field, Kind: ChangeRemove,
				Before: AsAny(b.ID),
			})
		case !bOK && pOK:
			out.Changes = append(out.Changes, Change{
				Field: field, Kind: ChangeAdd,
				After: AsAny(p),
			})
		default:
			// Enabled flag flip is the only per-row modify.
			// Pointer-aware: nil Enabled = "use default" (true per
			// the gregalemanifest convention), so compare
			// effective booleans.
			pendEnabled := true
			if p.Enabled != nil {
				pendEnabled = *p.Enabled
			}
			if pendEnabled != b.Enabled {
				out.Changes = append(out.Changes, Change{
					Field: field + ".enabled", Kind: ChangeModify,
					Before: AsAny(b.Enabled), After: AsAny(pendEnabled),
				})
			}
		}
	}
}

// diffEdgeRules compares the per-app edge rule list. Each rule's
// stable identity is (match_host, match_path, kind) — the gateway
// rebuilds the compiled slice on every change so even identical
// priority/methods count as a "modify".
func diffEdgeRules(out *Diff, base []api.EdgeRuleResponse, pending []api.CreateEdgeRuleRequest) {
	if pending == nil {
		return
	}
	type erKey struct{ host, path, kind string }
	baseByKey := map[erKey]api.EdgeRuleResponse{}
	for _, r := range base {
		baseByKey[erKey{r.MatchHost, r.MatchPath, r.Kind}] = r
	}
	pendByKey := map[erKey]api.CreateEdgeRuleRequest{}
	for i, r := range pending {
		pendByKey[erKey{r.MatchHost, r.MatchPath, r.Kind}] = pending[i]
	}

	allKeys := make([]erKey, 0, len(baseByKey)+len(pendByKey))
	for k := range baseByKey {
		allKeys = append(allKeys, k)
	}
	for k := range pendByKey {
		if _, ok := baseByKey[k]; !ok {
			allKeys = append(allKeys, k)
		}
	}
	sort.Slice(allKeys, func(i, j int) bool {
		if allKeys[i].kind != allKeys[j].kind {
			return allKeys[i].kind < allKeys[j].kind
		}
		if allKeys[i].host != allKeys[j].host {
			return allKeys[i].host < allKeys[j].host
		}
		return allKeys[i].path < allKeys[j].path
	})

	for _, k := range allKeys {
		b, bOK := baseByKey[k]
		p, pOK := pendByKey[k]
		idLabel := k.kind + " " + k.host + k.path
		switch {
		case bOK && !pOK:
			out.Changes = append(out.Changes, Change{
				Field: "edge_rule[" + idLabel + "]", Kind: ChangeRemove,
				Before: AsAny(b.ID),
			})
		case !bOK && pOK:
			out.Changes = append(out.Changes, Change{
				Field: "edge_rule[" + idLabel + "]", Kind: ChangeAdd,
				After: AsAny(p),
			})
		default:
			// Modify: action json differs (raw bytes compared
			// structurally) OR priority / methods / enabled
			// flipped. RawAction is json.RawMessage; EqualAny
			// compares the bytes.
			pendEnabled := true
			if p.Enabled != nil {
				pendEnabled = *p.Enabled
			}
			pendPriority := 0
			if p.Priority != nil {
				pendPriority = *p.Priority
			}
			actionChanged := !EqualAny(p.Action, b.Action)
			enabledChanged := pendEnabled != b.Enabled
			priorityChanged := pendPriority != b.Priority
			methodsChanged := !stringSliceEqual(p.MatchMethods, b.MatchMethods)
			if actionChanged || enabledChanged || priorityChanged || methodsChanged {
				out.Changes = append(out.Changes, Change{
					Field: "edge_rule[" + idLabel + "]", Kind: ChangeModify,
					Before: AsAny(b), After: AsAny(p),
				})
			}
		}
	}
}

// diffDeployment emits a single Break{ "would_create_deployment" }
// when any immutable field would change. Image, handler, and
// AppManifest fields (entrypoint, port, healthz, working_dir, user)
// are immutable per dto.go:1326.
func diffDeployment(out *Diff, base *api.DeploymentResponse, p Pending) {
	if base == nil {
		return
	}
	changes := []string{}
	if p.ImageRef != "" && p.ImageRef != base.ImageDigest {
		changes = append(changes, "image")
	}
	if p.Manifest != nil {
		if !stringSliceEqual(p.Manifest.Entrypoint, base.OverrideEntrypoint) {
			changes = append(changes, "entrypoint")
		}
		if p.Manifest.Port != 0 && p.Manifest.Port != base.OverridePort {
			changes = append(changes, "port")
		}
		baseHCPath := ""
		if base.OverrideHealthcheck != nil {
			baseHCPath = base.OverrideHealthcheck.Path
		}
		if p.Manifest.Healthz != "" && p.Manifest.Healthz != baseHCPath {
			changes = append(changes, "healthz")
		}
		// WorkingDir / User live on AppManifest (the per-app
		// scaffold payload surfaced via AppResponse.Manifest at
		// dto.go:441), not on the deployment overrides. The
		// deployment row only carries OverrideEntrypoint /
		// OverrideCmd / OverridePort / OverrideHealthcheck. We
		// intentionally do not emit WorkingDir / User diffs
		// here — the per-app AppManifest is a separate surface.
	}
	if len(changes) > 0 {
		out.Breaks = append(out.Breaks, Break{
			Code:     "would_create_deployment",
			Severity: "warn", // informational — not a quota break
			Reason:   "this would create a new deployment row, not patch the existing one (deployment fields are immutable post-create except min_instances).",
			Field:    "deployment",
			Observed: AsAny(changes),
		})
	}
}

// detectSchemaBreak is the PR-0 text-only schema-break signal.
// When handler / entrypoint / AppManifest.Env / AppManifest.EnvSecrets
// would change, the diff emits a Break with code
// "schema_response_changed". The full structural OpenAPI diff
// (Path/Method/Status/Schema walks) is PR-2.
//
// We deliberately emit one Break per affected field path so the
// renderer can show the customer exactly which response shapes
// might break. Multiple Break rows for one deploy is normal — the
// gate fires on the union.
func detectSchemaBreak(out *Diff, base *api.DeploymentResponse, p Pending) {
	if base == nil {
		return
	}
	if p.Manifest == nil {
		return
	}
	// Entrypoint change → process argv shifts; behaviour change is
	// possible.
	if !stringSliceEqual(p.Manifest.Entrypoint, base.OverrideEntrypoint) {
		out.Breaks = append(out.Breaks, Break{
			Code:     "schema_response_changed",
			Severity: "warn", // softer than handler — entrypoint
			// shifts don't always change response shape
			Reason: "entrypoint change can alter process behaviour",
			Field:  "entrypoint",
		})
	}
	// Env change → behaviour, not response shape — emit a warn so
	// the customer sees the env delta is meaningful, but don't
	// gate the deploy on it. We compare KEYS, not values: per
	// ADR-053 §Decision 4 the values are never echoed on the
	// deployment row (sealed / non-secret by contract), only the
	// OverrideEnvKeys []string set is wire-visible. A key add /
	// remove is the customer-visible signal.
	if len(p.Manifest.Env) > 0 {
		pendKeys := make([]string, 0, len(p.Manifest.Env))
		for k := range p.Manifest.Env {
			pendKeys = append(pendKeys, k)
		}
		if !stringSliceEqualAsSet(pendKeys, base.OverrideEnvKeys) {
			out.Breaks = append(out.Breaks, Break{
				Code:     "schema_env_changed",
				Severity: "warn",
				Reason:   "environment key set change can alter process behaviour",
				Field:    "manifest.env",
			})
		}
	}
	// EnvSecrets change → sealed-secret ref change. The wire form
	// carries OverrideEnvSecretRefs (map[string]string) — refs are
	// non-secret by design, so we CAN compare values here.
	if len(p.Manifest.EnvSecrets) > 0 && !stringMapsEqual(p.Manifest.EnvSecrets, base.OverrideEnvSecretRefs) {
		out.Breaks = append(out.Breaks, Break{
			Code:     "schema_env_changed",
			Severity: "warn",
			Reason:   "sealed-secret ref change can alter process behaviour",
			Field:    "manifest.env_secrets",
		})
	}
}

// keySet turns a slice into a map for fast lookup. Empty slice →
// empty map (never nil) so the caller doesn't need to nil-check.
func keySet(s []string) map[string]struct{} {
	out := make(map[string]struct{}, len(s))
	for _, k := range s {
		out[k] = struct{}{}
	}
	return out
}

// stringSliceEqual compares two []string for set equality (order
// independent). Used by egress_allowlist + cron match-methods
// diffs where the wire form is "first-seen-wins-dedup'd at write
// time" per dto.go:454.
func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ac := keySet(a)
	for _, x := range b {
		if _, ok := ac[x]; !ok {
			return false
		}
	}
	return true
}

// stringSliceEqualAsSet is the order-independent equivalent — used
// for env-key comparisons where the wire form may not guarantee
// order (OverrideEnvKeys is sorted at the apid handler seam, but
// the diff engine should not depend on that).
func stringSliceEqualAsSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ac := keySet(a)
	for _, x := range b {
		if _, ok := ac[x]; !ok {
			return false
		}
	}
	return true
}

// stringMapsEqual compares two map[string]string for equality. Used
// by AppManifest.Env / EnvSecrets diff where the wire form is a
// string→string map.
func stringMapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
