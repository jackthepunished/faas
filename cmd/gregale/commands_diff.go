// commands_diff.go — `gregale deploy --diff` CLI surface.
//
// Wires the deploy --diff flag into cmdDeployTarball via a
// short-circuit (commands2.go:1041+ after authedClient). The diff
// engine lives in pkg/deploydiff; this file is the CLI adapter
// that maps SDK reads into [pkg/deploydiff.Baseline] and CLI flags
// into [pkg/deploydiff.Pending], then calls the engine.
//
// The diff is read-only: it never calls CreateApp, Deploy, or
// DeployTarball. The only network traffic is five GETs (apps +
// deployments + envs + crons + edge-rules) plus the schema-break
// detection (text-only in PR-0; structural OpenAPI walk in PR-2).
//
// Exit codes:
//   - 0: no blocking breaks OR --lenient was set
//   - 1: at least one Break with severity "error" and --strict
//        (the default when --diff is set) is in effect
//
// --json emits the stable wire shape from pkg/deploydiff.RenderJSON.
// CI usage:
//
//	gregale deploy --diff --json | jq '.blocking'
//
// See plan in docs/plans/cozy-wishing-church.md (or whatever lands)
// for the server-side PR-1 extension (POST /v1/apps/{slug}/diff).

package main

import (
	"context"
	"errors"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/deploydiff"
	"github.com/onebox-faas/faas/pkg/gregalemanifest"
)

// diffCLIOptions is the parsed flag set carried from cmdDeployTarball
// into cmdDiff. Kept small so the diff path doesn't grow a
// second flag-set surface.
type diffCLIOptions struct {
	Slug     string
	AppShape shape
	Runtime  string
	Handler  string
	Image    string
	Cwd      string
	// AppConfigPatch mirrors the CLI flags that affect app-level
	// fields the diff should project (memory, concurrency,
	// require_authn, …). nil = "no change".
	AppConfig deploydiff.AppConfigPatch
	// Manifest is the would-write AppManifest for a fresh deploy
	// body. nil = "no manifest change proposed".
	Manifest *api.AppManifest
	// Crons is the post-deploy cron list (full-replacement).
	// Populated from the gregale.yaml triggers fan-out so the diff
	// shows "would create cron X" rows.
	Crons []api.CreateCronRequest
	// Diff flags.
	JSON    bool
	Strict  bool
	Lenient bool
	// ServerDiff routes through apid's POST /v1/apps/{slug}/diff
	// (PR-1). PR-0 leaves the flag wired but not yet implemented —
	// the local SDK path is the default until PR-1 lands.
	ServerDiff bool
}

// buildDiffOptions projects the parsed cmdDeployTarball flag set
// into the diff CLI options. Called from cmdDeployTarball's
// --diff short-circuit path so the diff sees the same flags a real
// deploy would.
func buildDiffOptions(slug string, sh shape, runtime, handler, image, cwd string, requireAuthnPtr *bool) diffCLIOptions {
	opts := diffCLIOptions{
		Slug:     slug,
		AppShape: sh,
		Runtime:  runtime,
		Handler:  handler,
		Image:    image,
		Cwd:      cwd,
	}
	if requireAuthnPtr != nil {
		v := *requireAuthnPtr
		opts.AppConfig.RequireAuthn = &v
	}
	return opts
}

// runDiff is the --diff short-circuit entry point. Called from
// cmdDeployTarball after the auth client is acquired but before
// any CreateApp / Deploy call. Returns the exit code.
func runDiff(ctx context.Context, client *api.Client, opts diffCLIOptions) int {
	// 1. Baseline snapshot.
	baseline, err := buildBaseline(ctx, client, opts.Slug)
	if err != nil {
		return printErr("Could not read baseline", err)
	}

	// 2. Pending projection. The gregale.yaml triggers fan-out
	// populates Pending.Crons (mirrors deployManifestTriggers).
	pending := buildPending(ctx, client, opts, baseline)

	// 3. Run the engine. The quota gate is a separate pass — the
	// engine itself doesn't read pkg/api/limits.go; the caller
	// supplies QuotaConfig.
	d := deploydiff.Compute(opts.Slug, baseline, pending)

	// 4. Plan + limits for the gate. When the plan is unknown
	// (PR-0 — the wire doesn't surface a full quota table today)
	// we skip the gate and emit a warn. Running the gate against a
	// zero-value Limits{} would false-fire on every Hobby/Pro/Scale
	// customer's existing config (see code-review finding #1 / #6).
	plan, limits, planKnown := inferPlanAndLimits(ctx, client, baseline)
	if planKnown {
		d.Plan = plan
		breaks := deploydiff.Quota(plan, baseline, pending, deploydiff.QuotaConfig{
			Limits:               limits,
			AccountCronCount:     accountCronCount(ctx, client, opts.Slug),
			AccountEdgeRuleCount: 0, // per-account edge-rule count not
			// currently capped; pass 0.
		})
		d.Breaks = append(d.Breaks, breaks...)
	} else {
		// Plan tier not resolved — apid's `Whoami` only surfaces a
		// partial AccountLimits table today (ram_mb, concurrency,
		// deployed_apps, included_gb_hours, app_layer_max_mb — no
		// crons / edge_rules / envs / etc.). Skip the gate and
		// emit a single warn so the customer's eye lands on it.
		d.Breaks = append(d.Breaks, deploydiff.Break{
			Code:     "plan_unknown_quota_gate_skipped",
			Severity: deploydiff.SeverityWarn,
			Reason:   "could not resolve plan tier from Whoami; quota gate skipped (PR-0). PR-1 ships a server-side plan lookup.",
			Field:    "plan",
		})
	}

	// 5. Render.
	if opts.JSON {
		if err := deploydiff.RenderJSON(osStdout, d); err != nil {
			return printErr("Could not encode diff", err)
		}
	} else {
		deploydiff.RenderText(osStdout, d)
	}

	// 6. Gate.
	if d.HasBlockingBreaks() && !opts.Lenient {
		return 1
	}
	return 0
}

// buildBaseline reads the live state for the slug. Missing app is
// not an error — a fresh deploy that would create-app has a
// zero-value baseline.
func buildBaseline(ctx context.Context, client *api.Client, slug string) (deploydiff.Baseline, error) {
	out := deploydiff.EmptyBaseline()
	app, err := client.GetApp(ctx, slug)
	switch {
	case err == nil:
		out.App = &app
		// Latest deployment: ListDeployments is account-scoped
		// (no ?app= filter today), so a single page can return
		// another app's most-recent row. Bound-paginate until we
		// find a row with AppID == app.ID, or until the cursor
		// (NextBefore) goes empty. The bound (maxDeploymentPages)
		// keeps the worst-case bounded so an account with hundreds
		// of apps doesn't loop forever; missing the match leaves
		// LatestDeployment nil — same shape as a never-deployed
		// app, which is the safe default.
		const pageSize = 20
		const maxDeploymentPages = 10 // ≤ 200 rows scanned worst-case
		cursor := ""
		pagesScanned := 0
		for pagesScanned < maxDeploymentPages {
			page, derr := client.ListDeployments(ctx, cursor, pageSize)
			if derr != nil {
				break // surface no error — LatestDeployment
				// staying nil is the safe default
			}
			for i := range page.Items {
				if page.Items[i].AppID == app.ID {
					latest := page.Items[i]
					out.LatestDeployment = &latest
					break
				}
			}
			if out.LatestDeployment != nil || page.NextBefore == "" {
				break
			}
			cursor = page.NextBefore
			pagesScanned++
		}
	case isNotFound(err):
		// Fresh deploy — leave baseline.App == nil.
	default:
		return out, err
	}
	// Env vars per scope. PR-0 uses the nested ?scope=__all__
	// shape; falls back to the flat default-scope list on the
	// pre-PR-D handler (defensive).
	envList, err := client.GetAppsSlugEnv(ctx, slug)
	if err != nil && !isNotFound(err) {
		return out, err
	}
	out.EnvByScope = deploydiff.EnvByScopeFromList(envList)
	// Crons.
	crons, err := client.ListCrons(ctx, slug)
	if err != nil && !isNotFound(err) {
		return out, err
	}
	out.Crons = crons
	// Edge rules.
	rules, err := client.ListEdgeRulesForApp(ctx, slug)
	if err != nil && !isNotFound(err) {
		return out, err
	}
	out.EdgeRules = rules
	return out, nil
}

// buildPending projects the parsed CLI flags + gregale.yaml into
// a [deploydiff.Pending]. The cron fan-out mirrors
// [deployManifestTriggers] but reads rather than writes.
func buildPending(ctx context.Context, client *api.Client, opts diffCLIOptions, baseline deploydiff.Baseline) deploydiff.Pending {
	p := deploydiff.Pending{AppConfig: opts.AppConfig}
	// Manifest: PR-0 synthesises a placeholder from the CLI flags
	// (image / handler). Real manifest extraction from the tarball
	// is the imaged contract — PR-0 keeps the diff text-only so
	// the gate can fire without spinning a build.
	if opts.Image != "" {
		p.ImageRef = opts.Image
	}
	// gregale.yaml triggers → crons.
	m, ok, err := gregalemanifest.Load(opts.Cwd)
	if err == nil && ok && m != nil {
		if verr := m.Validate(); verr == nil {
			for _, t := range m.Triggers {
				if t.Kind != gregalemanifest.TriggerKindCron {
					continue
				}
				if t.App != "" && t.App != opts.Slug {
					continue // one-app-at-a-time deploy
				}
				enabled := true
				if t.Enabled != nil {
					enabled = *t.Enabled
				}
				p.Crons = append(p.Crons, api.CreateCronRequest{
					Schedule: t.Schedule,
					Path:     t.Path,
					Enabled:  &enabled,
				})
			}
		}
	}
	return p
}

// inferPlanAndLimits resolves the account's plan tier + limits
// table. We piggyback on Whoami (which carries AccountLimits.Plan
// as a string) and lift that into the full [api.Limits] table via
// [api.MustLimitsFor]. The returned `bool` reports whether the
// lookup succeeded — when false, the caller must skip the gate
// and emit a single warn so the customer doesn't see phantom
// breaks (code-review finding #1 / #6).
//
// PR-1 will replace this with a server-side `GET /v1/account/limits`
// endpoint that returns the full quota table directly. Until then,
// Whoami is the canonical source for the plan tier.
func inferPlanAndLimits(ctx context.Context, client *api.Client, baseline deploydiff.Baseline) (api.Plan, api.Limits, bool) {
	acct, err := client.Whoami(ctx)
	if err != nil {
		return "", api.Limits{}, false
	}
	if acct.Plan == "" {
		return "", api.Limits{}, false
	}
	plan := api.Plan(acct.Plan)
	if !plan.Valid() {
		return "", api.Limits{}, false
	}
	limits := api.MustLimitsFor(plan)
	_ = baseline // reserved for PR-1's per-app upgrade
	return plan, limits, true
}

// accountCronCount reads the per-account cron count for the quota
// gate's CronLimitPerAccount check. PR-0 calls ListCrons with
// empty slug (= account-scoped) — the SDK already supports this
// per the ListCrons comment at client.go:944.
func accountCronCount(ctx context.Context, client *api.Client, excludeSlug string) int {
	rows, err := client.ListCrons(ctx, "")
	if err != nil {
		return 0
	}
	return len(rows)
}

// isNotFound matches the apid handler's 404 shape — used so a
// fresh deploy (no existing app) doesn't error out of
// buildBaseline. The SDK doesn't expose a typed sentinel yet;
// status-code match is the PR-0 contract.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var ae *api.APIError
	if errors.As(err, &ae) {
		return ae.Problem.Status == 404
	}
	return false
}
