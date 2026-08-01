package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/logsanitize"
	"github.com/onebox-faas/faas/pkg/state"
)

func (s *server) whoami(w http.ResponseWriter, r *http.Request, acct state.Account) {
	writeJSON(w, http.StatusOK, s.accountResponse(ctx(r), acct, r))
}

func (s *server) listApps(w http.ResponseWriter, r *http.Request, acct state.Account) {
	apps, err := s.store.ListApps(ctx(r), acct.ID)
	if err != nil {
		api.WriteProblem(w, api.ErrCapacity("could not list apps"))
		return
	}
	out := make([]api.AppResponse, 0, len(apps))
	for _, a := range apps {
		out = append(out, s.appResponse(a))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) createApp(w http.ResponseWriter, r *http.Request, acct state.Account) {
	var req api.CreateAppRequest
	if err := decodeJSON(r, &req); err != nil {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation, "Bad request", err.Error()))
		return
	}
	limits := api.MustLimitsFor(acct.Plan)
	app, prob := s.buildApp(acct, req, limits)
	if prob != nil {
		api.WriteProblem(w, prob)
		return
	}
	// Phase 2 / Gate A: pick the owner compute_node BEFORE the
	// quota transaction. The placement decision reads the
	// per-node live usedMB (state.ComputeNodeUsedMB) which is a
	// snapshot of the instances table; running placement inside
	// the FOR UPDATE account lock would let two concurrent
	// creates on different accounts see each other's writes
	// while serialising on the same account. The lock only
	// protects the deployed-app cap; placement uses a fresh
	// read at create time. The fresh read is good enough
	// because the chooser is re-validated by the schedd's
	// per-instance NodeLedger at first wake (the runtime gate).
	node, prob := s.placement.Choose(ctx(r), "", app.RAMMB)
	if prob != nil {
		// The placement helper returns *api.Problem directly —
		// capacity refusals land as 503, internal failures as
		// 500. Customer-facing shape matches the pre-Phase-2
		// chooser (no shape change at the wire).
		api.WriteProblem(w, prob)
		return
	}
	app.NodeID = node.ID
	// Deployed-app count quota + insert happen in the same critical
	// section inside the store (PgStore: SELECT … FOR UPDATE on the
	// parent accounts row; MemStore: m.mu). This closes the TOCTOU the
	// previous CountDeployedApps + CreateApp pair exposed on Free/Hobby
	// accounts under concurrency (spec §4.2).
	created, err := s.store.CreateAppIfUnderQuota(ctx(r), app, limits)
	if err != nil {
		var qe *state.QuotaError
		switch {
		case errors.As(err, &qe):
			api.WriteProblem(w, api.ErrPlanLimitApps(limits, qe.Observed))
		case errors.Is(err, state.ErrConflict):
			api.WriteProblem(w, api.NewProblem(http.StatusConflict, api.CodeValidation,
				"Slug taken", fmt.Sprintf("app slug %q is already in use", req.Slug)))
		default:
			api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError, api.CodeCapacity,
				"Capacity", "could not create app"))
		}
		return
	}
	// CodeQL go/log-injection (CWE-117): created.Slug passes validSlug's
	// regex check before persist (^[a-z0-9]([a-z0-9-]{1,38})[a-z0-9]$),
	// but CodeQL's taint engine doesn't model that sanitizer. Wrap the
	// slug in logsanitize.Field so the audit line is one-event-per-line
	// regardless of what a future refactor of validSlug does.
	s.log.Info("app created", "app", created.ID, "slug", logsanitize.Field(created.Slug), "account", acct.ID)
	// IAM-4 (issue #291): record the app creation. Runtime is
	// omitted from the data map when empty (AppTypeApp has no
	// runtime) so the row stays minimal. The audit row never
	// reaches the structured-log sink, so logsanitize is not
	// needed here — CodeQL go/log-injection only fires on slog.
	s.audit.Emit(ctx(r), "app.created", &acct.ID, map[string]any{
		"app_id":          created.ID,
		"slug":            created.Slug,
		"type":            string(created.Type),
		"ram_mb":          created.RAMMB,
		"max_concurrency": created.MaxConcurrency,
		"runtime":         created.Runtime,
	})
	writeJSON(w, http.StatusCreated, s.appResponse(created))
}

// buildApp applies defaults and validates a create request, returning the App to
// persist or a *Problem describing the first violation.
func (s *server) buildApp(acct state.Account, req api.CreateAppRequest, limits api.Limits) (state.App, *api.Problem) {
	if !validSlug(req.Slug) {
		return state.App{}, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
			"Invalid slug", "slug must be 3–40 chars, lowercase letters, digits, and hyphens")
	}
	typ := state.AppType(orDefault(req.Type, string(state.AppTypeApp)))
	if typ != state.AppTypeApp && typ != state.AppTypeFunction {
		return state.App{}, api.NewProblem(http.StatusBadRequest, api.CodeValidation, "Invalid type", "type must be app or function")
	}
	if typ == state.AppTypeFunction && req.Runtime != "node22" && req.Runtime != "python312" && req.Runtime != "go124" && req.Runtime != "go124-alpine" && req.Runtime != "node24" && req.Runtime != "python313" {
		return state.App{}, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
			"Invalid runtime", "functions require runtime node22, python312, go124, go124-alpine, node24, or python313")
	}
	ram := req.RAMMB
	if ram == 0 {
		ram = limits.RAMMB
	}
	mc := req.MaxConcurrency
	if mc == 0 {
		mc = 1
	}
	if prob := api.ValidateAppConfig(limits, ram, mc); prob != nil {
		return state.App{}, prob
	}
	// Issue #471 / ADR-047: per-app streaming flag. Apply the
	// plan-level default when the request didn't carry one — a
	// Hobby customer's brand-new app is streaming-ready without an
	// extra PATCH round-trip. Free defaults to false (the only
	// legal value on Free; apid rejects PATCH true with 403
	// plan_streaming_not_allowed). The Plan accessor keeps the
	// fail-closed contract (pkg/api/limits.go) — Free's accessor
	// returns false just like LimitsFor(false) would.
	streaming := acct.Plan.StreamingEnabled()
	if req.StreamingEnabled != nil {
		streaming = *req.StreamingEnabled
	}
	return state.App{
		AccountID: acct.ID, Slug: req.Slug, Type: typ, Runtime: req.Runtime,
		RAMMB: ram, MaxConcurrency: mc, IdleTimeoutS: req.IdleTimeoutS, Status: state.AppActive,
		StreamingEnabled: streaming,
	}, nil
}

func (s *server) createDeployment(w http.ResponseWriter, r *http.Request, acct state.Account) {
	// DeployedApps (the per-account cap on apps) is enforced at app-create
	// time via store.CreateAppIfUnderQuota — the deploy path cannot
	// bypass it because the parent apps row must already exist. The
	// active-app gate that prevents an orphan deployment row pointing
	// at a soft-deleted app lives inside store.CreateDeployment
	// (PR-A: SELECT 1 FROM apps WHERE id=$1 AND status='active' FOR UPDATE).
	// If the app was deleted between this AppBySlug and the
	// CreateDeployment INSERT, the store returns ErrNotFound and we
	// surface the same 404 as a missing slug.
	app, err := s.store.AppBySlug(ctx(r), r.PathValue("slug"))
	if err != nil || app.AccountID != acct.ID {
		s.notFound(w, "no such app")
		return
	}
	ct := r.Header.Get("Content-Type")
	// Lookup the plan limits once. Both branches need them:
	//   - multipart: cap upload size at SourceTarballMaxMB
	//   - JSON:     validate override env byte + count caps
	// Hoisting avoids a second MustLimitsFor call in the override
	// branch (issue #460 / ADR-053).
	limits := api.MustLimitsFor(acct.Plan)
	if strings.HasPrefix(ct, "multipart/form-data") {
		// Cap upload size at the plan's SourceTarballMaxMB before any
		// multipart parsing — MaxBytesReader returns a *MaxBytesError on
		// overflow, which r.MultipartReader surfaces as a parse error that
		// createDeploymentMultipart already maps to 413. The pre-Check
		// in deploy_inputs.go only fires when ContentLength is known.
		max := int64(limits.SourceTarballMaxMB) * 1024 * 1024
		r.Body = http.MaxBytesReader(w, r.Body, max)
		s.createDeploymentMultipart(w, r, acct, app)
		return
	}
	var req api.CreateDeploymentRequest
	if err := decodeJSON(r, &req); err != nil {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation, "Bad request", err.Error()))
		return
	}
	if !isDigestPinned(req.Image) {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeImageRequired,
			"Image required", "image: deploys require a digest-pinned reference, e.g. registry.gregale.dev/app@sha256:..."))
		return
	}
	// Issue #472 / ADR-054: pre-flight signature-enforcement gate.
	// Runs after the digest check (a missing image is a more
	// fundamental request shape error than a missing signer) and
	// before the override Validate (so a fail-closed deployment
	// doesn't pay the override-validation cost). The flag is on
	// the apps row (apps.require_signed); we do NOT trust the
	// customer's req.RequireSigned opt-in to override the operator
	// policy — the per-app flag wins (fail-closed). A customer
	// attempt to clear an operator-on flag is rejected here:
	//
	//   app.require_signed=true  &  req.RequireSigned=*false   → 403
	//
	// The "no trusted signers configured" check is the actual
	// fail-closed trip — the operator toggled the flag but never
	// onboarded a publisher. We surface this immediately at
	// accept-time so the customer sees 403 with a clear message,
	// not a pending→failed two-step inside imaged.
	if app.RequireSigned {
		signers, sErr := s.store.ListAppTrustedSigners(ctx(r), acct.ID, app.ID)
		if sErr != nil {
			api.WriteProblem(w, api.ErrCapacity("could not load trusted signers"))
			return
		}
		if len(signers) == 0 {
			api.WriteProblem(w, api.ErrDeploySignatureInvalid(
				"apps.require_signed=true but no trusted publishers are configured for this app; ask the operator to onboard a publisher via PUT /v1/apps/{slug}/trusted_signers/{name}."))
			return
		}
		// Customer-request override: an attempt to turn the flag
		// off on this single deploy is rejected with operator >
		// customer. A nil request field is "leave the per-app flag
		// alone" (the apid default); *true is a no-op (the flag
		// is already on); only *false collides.
		if req.RequireSigned != nil && !*req.RequireSigned {
			api.WriteProblem(w, api.ErrDeploySignatureInvalid(
				"apps.require_signed=true on this app; per-deploy opt-out is not permitted (operator policy wins)."))
			return
		}
	}
	// Issue #460 / ADR-053: validate the override object AFTER the
	// image digest check (digest first — a missing image is a more
	// fundamental request shape error than a malformed override).
	// A failed override validation 400s the whole request — the
	// override is NEVER silently dropped (ADR-053 §Decision 2).
	// Plan tier comes from the authenticated account (limits already
	// hoisted above the multipart branch).
	var overrides *api.CreateDeploymentOverrides
	if req.Overrides != nil {
		if p := req.Overrides.Validate(limits); p != nil {
			api.WriteProblem(w, p)
			return
		}
		overrides = req.Overrides
	}
	// PR-B: the prior-deployment supersede is folded into
	// store.CreateDeployment's tx (pkg/state/pgstore.go). apid no longer
	// holds a "supersede then create" two-step — the in-tx ordering
	// guarantees the previous live deployment is NEVER observed
	// superseded without the new pending row also being visible. We
	// read the prior row BEFORE the call via LatestDeployment so the
	// supersede-notify can carry its id; this keeps the return shape
	// 2-tuple and backward-compatible with pre-PR-B call sites.
	prev, _ := s.store.LatestDeployment(ctx(r), app.ID)
	dep := state.Deployment{
		AppID: app.ID, ImageDigest: req.Image, Kind: state.DeploymentKindImage, Status: state.DeployPending,
	}
	if overrides != nil {
		// Convert the validated override into the DB shape. The
		// json.RawMessage columns carry the marshalled map; nil
		// means "no override" (the store writes NULL).
		if len(overrides.Entrypoint) > 0 {
			dep.OverrideEntrypoint = overrides.Entrypoint
		}
		if len(overrides.Cmd) > 0 {
			dep.OverrideCmd = overrides.Cmd
		}
		if len(overrides.Env) > 0 {
			if b, err := json.Marshal(overrides.Env); err == nil {
				dep.OverrideEnv = b
			}
		}
		if len(overrides.EnvSecrets) > 0 {
			if b, err := json.Marshal(overrides.EnvSecrets); err == nil {
				dep.OverrideEnvSecrets = b
			}
		}
		if overrides.Port != 0 {
			dep.OverridePort = overrides.Port
		}
		if overrides.Healthcheck != nil {
			if b, err := json.Marshal(overrides.Healthcheck); err == nil {
				dep.OverrideHealthcheck = b
			}
		}
	}
	d, err := s.store.CreateDeployment(ctx(r), dep)
	if err != nil {
		api.WriteProblem(w, api.ErrCapacity("could not create deployment"))
		return
	}
	// IAM-2 (issue #186): 2nd-deploy chokepoint. The deployment
	// row is now visible; CountDeployments reflects this one
	// (the SQL filter excludes failed/superseded). If the new
	// count is >= 2, the customer crossed the threshold on
	// this deploy — arm mfa_required for the next login.
	s.maybeFlipMFAOnDeploy(ctx(r), acct)
	// F-03: deployment_changed emits now carry status + deployment_id.
	// status="pending" tells listeners this row is still in-flight (builderd
	// will eventually stamp rootfs_path → imaged converts to ext4); later
	// transitions re-emit with status="live"|"failed"|"superseded".
	// deployment_id==to here, but imaged switches on deployment_id in
	// handleDeployment. Apid does not synthesise every transition — the
	// state machine walks pending→building→imaging→snapshotting→live and
	// each row write is followed by a NotifyDeploymentChanged. The image
	// branch below covers the first hop (submitted); later hops land in
	// cmd/apid/deploy_steps.go.
	_ = s.notif.Notify(ctx(r), db.NotifyDeploymentChanged,
		fmt.Sprintf(`{"kind":"image","status":"pending","app_id":"%s","deployment_id":"%s","to":"%s"}`, app.ID, d.ID, d.ID))
	// PR-B: if a prior row was just superseded inside the same tx,
	// fire a second NotifyDeploymentChanged so imaged's F5 cleanup
	// handler (handleDeploymentChanged) can drop the prior snapshot.
	// The notify carries status="superseded" + to=prev.ID; if no prev
	// existed (first deploy on this app), skip the second notify.
	if prev.ID != "" {
		_ = s.notif.Notify(ctx(r), db.NotifyDeploymentChanged,
			fmt.Sprintf(`{"kind":"image","status":"superseded","app_id":"%s","deployment_id":"%s","to":"%s"}`, app.ID, prev.ID, prev.ID))
	}
	// Sanitize req.Image at the log sink — CodeQL go/log-injection (CWE-117).
	// isDigestPinned already rejects malformed refs with 400 before this line,
	// but a future field/wrapper change would break that invariant. Sanitizing
	// here means the log statement stays safe regardless of upstream changes.
	// d.ID and app.ID are server-generated UUIDs — no sanitize needed.
	s.log.Info("deployment created", "deployment", d.ID, "app", app.ID, "ref", logsanitize.Field(req.Image))
	// IAM-4 (issue #291): record the deployment. data.supersedes
	// is the previous deployment_id (PR-B: read before the
	// CreateDeployment tx via LatestDeployment, line 167 in the
	// pre-PR-#340 layout). Empty when this is the first deploy
	// on the app — dashboards can distinguish "first deploy"
	// from "supersede" without inspecting app history.
	//
	// Issue #460 / ADR-053: data.has_overrides is the audit-side
	// mirror of the HasOverrides response field. Set true when the
	// deployment carried any override_* column. The override values
	// themselves are NEVER in the audit payload — only the boolean
	// (ADR-053 §Decision 4 + ADR-045 §Decision 6 mirror: env values
	// never cross the audit sink).
	hasOverrides := req.Overrides != nil
	s.audit.Emit(ctx(r), "app.deployed", &acct.ID, map[string]any{
		"app_id":        app.ID,
		"deployment_id": d.ID,
		"ref":           req.Image,
		"supersedes":    prev.ID,
		"has_overrides": hasOverrides,
	})
	// Issue #472 / ADR-054: emit app.signed_image_accepted here ONLY
	// when require_signed is on for this deploy. imaged will later
	// emit app.signature_invalid / app.signature_missing from its
	// verify hook (Bucket 4), but the "request passed the operator
	// gate" event is apid's surface — the deploy is acked before
	// imaged even runs the verify. The audit row answers "which
	// signature-gated deploy was accepted on what app" without a
	// follow-up GET. Empty ref column keeps the row distinct from
	// the plain app.deployed event (different `kind`).
	if app.RequireSigned {
		s.audit.Emit(ctx(r), "app.signed_image_accepted", &acct.ID, map[string]any{
			"app_id":        app.ID,
			"deployment_id": d.ID,
			"ref":           req.Image,
		})
	}
	writeJSON(w, http.StatusAccepted, s.deploymentResponse(d))
}

func (s *server) appResponse(a state.App) api.AppResponse {
	// EgressAllowlist is materialised as a non-nil empty slice so
	// the JSON shape is `[]` (never `null`) regardless of plan /
	// pre-PATCH state. prefix.String() is the canonical form
	// ("1.2.3.0/24", "fe80::/10"); validateUpdateApp has already
	// rewritten any "::ffff:" v4-mapped entry to its v4 form by
	// the time it lands in the store, so we never see one here.
	ea := egressStringList(a.EgressAllowlist)
	return api.AppResponse{
		ID: a.ID, Slug: a.Slug, Type: string(a.Type), Runtime: a.Runtime,
		RAMMB: a.RAMMB, MaxConcurrency: a.MaxConcurrency, IdleTimeoutS: a.IdleTimeoutS,
		// ux_spec §6.5: per-app floor the reaper honors when
		// parking idle instances. Pro/Scale only (apid gates).
		MinInstances: a.MinInstances,
		Status:       string(a.Status), URL: fmt.Sprintf("https://%s.apps.%s", a.Slug, s.domain),
		Manifest: api.AppManifest{
			Entrypoint: a.Manifest.Entrypoint,
			Env:        a.Manifest.Env,
			WorkingDir: a.Manifest.WorkingDir,
			Port:       a.Manifest.Port,
			Healthz:    a.Manifest.Healthz,
			User:       a.Manifest.User,
		},
		EgressAllowlist: ea,
		// Issue #169 / #172: per-app reactive scale-up trigger
		// targets. 0 = "disabled" (no autoscale rule). Reactive
		// scale-up runs in pkg/sched/scaleup; the trigger reads
		// these columns every tick.
		AutoscaleTargetRPS:    a.AutoscaleTargetRPS,
		AutoscaleTargetCPUPct: a.AutoscaleTargetCPUPct,
		// Issue #471 / ADR-047: per-app streaming flag. Surfaced so
		// dashboards can show "streaming on / off" alongside the
		// egress-allowlist flag.
		StreamingEnabled: a.StreamingEnabled,
		// Issue #462 / ADR-058 / PR-A: per-app scaling policy. nil
		// = legacy row (projected from min_instances / max_concurrency
		// by the read path). Non-nil = customer-authored policy.
		// The state layer is the canonical source; the DTO carries
		// the same shape so the dashboard / CLI surface one
		// consistent struct.
		ScalingPolicy:  statePolicyToDTO(a.ScalingPolicy),
		LastScaleOutAt: a.LastScaleOutAt,
		LastScaleInAt:  a.LastScaleInAt,
		// Issue #472 / ADR-054: per-app signature-enforcement flag.
		// Surfaced so dashboards can show "signature required" alongside
		// the streaming flag, and so a customer can verify their
		// PATCH landed without a second round-trip.
		RequireSigned: a.RequireSigned,
	}
}

// statePolicyToDTO converts the state-layer `*state.ScalingPolicy`
// to the wire DTO `*api.ScalingPolicy`. Returns nil when the input
// is nil so legacy rows project as a JSON `null` (the pre-#462
// contract). Target is pointer-to-pointer so a customer-authored
// `Target: {metric: "rps", value: 0}` round-trips through the read
// path with the metric intact (the pre-fix path dropped Target when
// Value==0, which the DTO upgrade to pointer-Target preserves).
func statePolicyToDTO(p *state.ScalingPolicy) *api.ScalingPolicy {
	if p == nil {
		return nil
	}
	out := &api.ScalingPolicy{
		MinInstances:      p.MinInstances,
		MaxInstances:      p.MaxInstances,
		ScaleOutCooldownS: p.ScaleOutCooldownS,
		ScaleInCooldownS:  p.ScaleInCooldownS,
	}
	if p.Target != nil {
		out.Target = &api.ScalingTarget{
			Metric: p.Target.Metric,
			Value:  p.Target.Value,
		}
	}
	return out
}

// accountResponse builds the AccountResponse DTO, populating Limits
// (plan caps), AppCount (deployed apps), and UsageGBHours (current
// calendar month). Errors from store reads are swallowed — best
// effort; the dashboard renders the row even when the meter is
// temporarily unavailable (meterd republishes every minute).
//
// GitHubInstall is left empty for now; slice 8 fills it from
// githubd's bindings table once the daemon is live.
func (s *server) accountResponse(ctx context.Context, acct state.Account, r *http.Request) api.AccountResponse {
	l := api.MustLimitsFor(acct.Plan)
	resp := api.AccountResponse{
		ID:     acct.ID,
		Email:  acct.Email,
		Plan:   string(acct.Plan),
		Status: string(acct.Status),
		Limits: api.AccountLimits{
			Plan:            string(acct.Plan),
			RAMMB:           l.RAMMB,
			MaxConcurrency:  l.MaxConcurrency,
			DeployedApps:    l.DeployedApps,
			IncludedGBHours: int64(l.IncludedGBHours),
			AppLayerMaxMB:   l.AppLayerMaxMB,
		},
	}
	if r != nil {
		if n, err := s.store.CountDeployedApps(ctx, acct.ID); err == nil {
			resp.AppCount = n
		}
		month := time.Now().UTC()
		if rows, err := s.store.UsageByMonth(ctx, acct.ID, month); err == nil {
			var mbSec int64
			for _, u := range rows {
				mbSec += u.MBSeconds
			}
			resp.UsageGBHours = float64(mbSec) / 3_600_000.0
		}
	}
	return resp
}

var slugRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{1,38})[a-z0-9]$`)

func validSlug(s string) bool { return slugRe.MatchString(s) }

// digestPinnedRE matches a digest-pinned OCI reference end-to-end:
//
//	<host>[/<repo-path>]/<name>@sha256:<64 lowercase hex>
//
// Where:
//
//	host     = RFC 1123 hostname (alnum + '-', dot-separated labels,
//	           optional :<port>)
//	repo     = alnum + '_-' + '.' + '/' (the OCI repository path grammar)
//
// The whole-ref anchoring is load-bearing: parseImageDigest feeds
// apid.createDeployment's slog log of req.Image (CodeQL go/log-injection),
// so a substring-search validator that only verifies the digest tail would
// let any non-OCI prefix through (including control chars / whitespace /
// extra @-separators). The host charset forbids control chars and
// whitespace explicitly, so the entire accepted string is printable OCI.
var digestPinnedRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*(:[0-9]+)?/[A-Za-z0-9_./-]+@sha256:[0-9a-f]{64}$`)

// parseImageDigest requires a digest-pinned reference (spec gap G1: public
// registries, digest-pinned) and returns the digest portion (sha256:...).
func parseImageDigest(ref string) (string, bool) {
	if !digestPinnedRE.MatchString(ref) {
		return "", false
	}
	return ref[strings.Index(ref, "@"):], true
}

// isDigestPinned reports whether ref is a digest-pinned reference (the form
// the deploy contract requires). Use this for input validation; consumers
// parse the full ref via oci.ParseReference so they can dial the right
// registry host.
func isDigestPinned(ref string) bool {
	return digestPinnedRE.MatchString(ref)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// egressStringList renders a stored []netip.Prefix to its canonical
// string form ("1.2.3.0/24", "fe80::/10"). The empty case returns a
// non-nil zero-length slice so the JSON shape is `[]` (never `null`)
// regardless of the plan / pre-PATCH state. validateUpdateApp has
// already rewritten any "::ffff:" v4-mapped entry to its v4 form by
// the time it lands in the store, so we never see one here. Reused by
// the audit emit (handlers_ext.go::updateApp) so the wire shape and
// the audit row agree on the canonical form.
func egressStringList(prefixes []netip.Prefix) []string {
	out := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		out = append(out, p.String())
	}
	return out
}
