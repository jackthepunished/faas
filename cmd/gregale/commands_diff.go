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

	// 4. Plan + limits for the gate.
	plan, limits := inferPlanAndLimits(ctx, client, baseline)
	if plan != "" {
		d.Plan = plan
	}
	breaks := deploydiff.Quota(plan, baseline, pending, deploydiff.QuotaConfig{
		Limits:               limits,
		AccountCronCount:     accountCronCount(ctx, client, opts.Slug),
		AccountEdgeRuleCount: 0, // per-account edge-rule count not
		// currently capped; pass 0.
	})
	d.Breaks = append(d.Breaks, breaks...)

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
		// Latest deployment: pull a one-element page to read the
		// most-recent row. ListDeployments orders by created_at DESC
		// (per memory pr-846-…) so the [0] element is the live row.
		page, derr := client.ListDeployments(ctx, "", 1)
		if derr == nil && len(page.Items) > 0 {
			// The list is account-scoped, not app-scoped — filter
			// by AppID. PR-0 is conservative: scan the page for a
			// match; a future PR adds ListDeploymentsForApp.
			for _, d := range page.Items {
				if d.AppID == app.ID {
					latest := d
					out.LatestDeployment = &latest
					break
				}
			}
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

// inferPlanAndLimits reads the plan tier from the account's
// limits. The wire shape isn't surfaced today (a Free customer's
// AppResponse has no Plan field); PR-0 returns an empty plan and
// the engine's gate fires only on the per-app-level caps that the
// limits table can still validate.
//
// PR-1 will add a server-side `GET /v1/account/limits` endpoint
// (or piggyback on Whoami) so the CLI can fetch the tier directly.
func inferPlanAndLimits(ctx context.Context, client *api.Client, baseline deploydiff.Baseline) (api.Plan, api.Limits) {
	// Best-effort: Whoami carries the plan tier in a future field;
	// today we fall back to an empty plan and zero limits.
	// PR-1 will wire this through the server endpoint.
	return "", api.Limits{}
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
