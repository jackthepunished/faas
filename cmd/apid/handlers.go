package main

import (
	"context"
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
		out = append(out, s.appResponse(a, acct.Plan))
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
	// Phase 2 / Gate A: leave node_id NULL — schedd's
	// PlacementClaimSubscriber stamps the owner (see emitAppCreated
	// for the full architectural rationale + docs/adr/055).
	app.NodeID = ""
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
	s.log.Info("app created", "app", created.ID, "slug", logsanitize.Field(created.Slug), "account", acct.ID)
	s.audit.Emit(ctx(r), "app.created", &acct.ID, map[string]any{
		"app_id":          created.ID,
		"slug":            created.Slug,
		"type":            string(created.Type),
		"ram_mb":          created.RAMMB,
		"max_concurrency": created.MaxConcurrency,
		"runtime":         created.Runtime,
	})
	s.emitAppCreated(ctx(r), created)
	writeJSON(w, http.StatusCreated, s.appResponse(created, acct.Plan))
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
	// Issue #470 / ADR-055: per-app two-tier snapshot flag. Apply
	// the plan-level default when the request didn't carry one —
	// a Pro customer's brand-new app gets warm.snap capture
	// without an extra PATCH round-trip. Free/Hobby default to
	// false (the only legal value on those tiers; apid rejects
	// PATCH-true with 403 plan_warm_snapshot_not_allowed). The
	// per-request override on Pro/Scale lets a customer opt out
	// (e.g. an app they know runs cold every request).
	warmEnabled := acct.Plan.WarmSnapshotEnabled()
	if req.WarmSnapshotEnabled != nil {
		warmEnabled = *req.WarmSnapshotEnabled
	}
	// Issue #560 + issue #695 / ADR-080: per-app require_authn
	// + public_auth_mode. The default is now per-plan — see
	// pkg/api/limits.go (Plan.RequireAuthnDefault +
	// Plan.PublicAuthModeDefault). Per-plan truth table:
	// Free={false, "open"}, Hobby={true, "open"},
	// Pro={true, "bearer"}, Scale={true, "bearer"}. Existing
	// customers are unaffected because migration 00155
	// grand-fathered every pre-flip row with
	// auth_default_flipped_at and did NOT flip their
	// require_authn / public_auth_mode values. The plan gate
	// for an explicit PATCH-true still fires at the PATCH
	// handler (issue #560 + ADR-079), not at Create time — a
	// Free customer's CreateApp call with require_authn=true
	// still gets 403 plan_require_authn_not_allowed via the
	// standard plan-gate shape. The per-request override on
	// Pro/Scale lets a customer opt out at create time (e.g.
	// a Pro customer's brand-new staging app that wants the
	// public path), and a Hobby customer can PATCH-false
	// immediately after creation to keep the public path.
	requireAuthn := acct.Plan.RequireAuthnDefault()
	if req.RequireAuthn != nil {
		requireAuthn = *req.RequireAuthn
	}
	// public_auth_mode has no wire-side override on
	// CreateAppRequest today — PATCH is the only surface for
	// it (the basic-cred sealing step requires plaintext
	// credentials, which only makes sense on a PATCH
	// roundtrip). The default lands here so a freshly
	// created Hobby/Pro/Scale app inherits the gate as part
	// of the secure-by-default flip.
	publicAuthMode := acct.Plan.PublicAuthModeDefault()
	// Apply the per-app threshold defaults from the plan; an
	// explicit override on the request wins. Out-of-range values
	// were already rejected at the JSON-decode layer
	// (api.ValidateWarmSnapshotBounds), so the only path that
	// produces an out-of-range value here is a buggy test or
	// internal caller.
	warmMinReqs := acct.Plan.WarmSnapshotMinRequestsDefault()
	if req.WarmSnapshotMinRequests != nil {
		warmMinReqs = *req.WarmSnapshotMinRequests
	}
	warmMinMs := acct.Plan.WarmSnapshotMinMsDefault()
	if req.WarmSnapshotMinMs != nil {
		warmMinMs = *req.WarmSnapshotMinMs
	}
	return state.App{
		AccountID: acct.ID, Slug: req.Slug, Type: typ, Runtime: req.Runtime,
		RAMMB: ram, MaxConcurrency: mc, IdleTimeoutS: req.IdleTimeoutS, Status: state.AppActive,
		StreamingEnabled:    streaming,
		WarmSnapshotEnabled: warmEnabled,
		// Issue #560 + issue #695 / ADR-080: see the
		// plan-default block above. Default is per-plan
		// (Plan.RequireAuthnDefault + Plan.PublicAuthModeDefault);
		// the per-plan gate (RequireAuthnAllowed +
		// PublicAuthBearerAllowed) is consulted only when an
		// existing app is PATCHed, not at Create time. State
		// layer is the canonical source (apps.require_authn +
		// apps.public_auth_mode columns); the DTO surfaces
		// the same values.
		RequireAuthn:   requireAuthn,
		PublicAuthMode: publicAuthMode,
		// Coerce to the plan minimums when the request asked for a
		// warm config but the plan says warm-snapshot is off: the
		// store ignores them anyway (the cold-boot path doesn't
		// read min_requests / min_ms), and the apid response
		// projects the plan defaults so dashboards stay consistent.
		WarmSnapshotMinRequests: warmMinReqs,
		WarmSnapshotMinMs:       warmMinMs,
	}, nil
}

func (s *server) createDeployment(w http.ResponseWriter, r *http.Request, acct state.Account) {
	// DeployedApps is enforced at app-create time; the active-app
	// gate lives in store.CreateDeployment (returns ErrNotFound on a
	// soft-deleted app, which we surface as 404 here). Multipart
	// uploads go down the createDeploymentMultipart branch; the
	// JSON branch is the rest of this handler. Extracted to
	// loadAppAndPreflight so createDeployment stays under the
	// CLAUDE.md 50-line handler cap.
	app, ok, limits := s.loadAppAndPreflight(w, r, acct)
	if !ok {
		return
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		// Cap upload size at the plan's SourceTarballMaxMB before any
		// multipart parsing — MaxBytesReader returns a *MaxBytesError on
		// overflow which createDeploymentMultipart maps to 413.
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
	// Pre-CreateDeployment validation gates (#472 / #460 / #463).
	// Gate order matters (signature → override → sidecar); each
	// helper short-circuits only on its own failure.
	if p := enforceSignatureGate(ctx(r), s, acct, app, &req); p != nil {
		api.WriteProblem(w, p)
		return
	}
	overrides, p := validateOverrides(&req, limits)
	if p != nil {
		api.WriteProblem(w, p)
		return
	}
	if p := validateAndPlanSidecars(&req, acct, limits); p != nil {
		api.WriteProblem(w, p)
		return
	}
	// PR-B: prior-deployment supersede is in store.CreateDeployment's tx;
	// we read prev BEFORE the call so the supersede-notify can carry
	// its id (LatestDeployment returns the post-supersede row).
	prev, _ := s.store.LatestDeployment(ctx(r), app.ID)
	dep, sErr := buildDeploymentForInsert(app, &req, overrides, limits)
	if sErr != nil {
		api.WriteProblem(w, sErr)
		return
	}
	d, err := s.store.CreateDeployment(ctx(r), dep)
	if err != nil {
		api.WriteProblem(w, api.ErrCapacity("could not create deployment"))
		return
	}
	// IAM-2 2nd-deploy chokepoint: arm mfa_required when this is the
	// customer's 2nd live deployment. Post-CreateDeployment notify +
	// audit + log fan-out is in notifyAndAuditDeployment.
	s.maybeFlipMFAOnDeploy(ctx(r), acct)
	notifyAndAuditDeployment(ctx(r), s, acct, app, d, prev, &req)
	writeJSON(w, http.StatusAccepted, s.deploymentResponse(d))
}

// loadAppAndPreflight resolves the app from the URL slug, enforces
// IDOR (app.AccountID == acct.ID), and hoists the per-plan limits.
// On any failure path, writes the appropriate error response and
// returns ok=false; on success returns (app, true, limits).
//
// Extracted from createDeployment (handlers.go) so the handler stays
// under the CLAUDE.md 50-line cap. The IDOR check is identical to
// loadApp in auth_facade.go; the difference is we ALSO return the
// per-plan limits, which createDeployment needs both for the
// multipart-source-tarball cap and the override / sidecar
// validators.
func (s *server) loadAppAndPreflight(w http.ResponseWriter, r *http.Request, acct state.Account) (state.App, bool, api.Limits) {
	app, err := s.store.AppBySlug(ctx(r), r.PathValue("slug"))
	if err != nil || app.AccountID != acct.ID {
		s.notFound(w, "no such app")
		return state.App{}, false, api.Limits{}
	}
	return app, true, api.MustLimitsFor(acct.Plan)
}

// appResponse converts a state.App row into the wire DTO. The plan
// is threaded through so the DTO can surface plan-derived caps
// (issue #559: ConcurrencyPerVMBound) without re-looking-up the
// account store. Mirrors how loadAppAndPreflight (above) threads
// (state.App, api.Limits) — every caller has acct in scope.
func (s *server) appResponse(a state.App, plan api.Plan) api.AppResponse {
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
		// Issue #559: platform-advertised per-VM concurrency cap
		// for the customer's plan. Distinct from MaxConcurrency
		// (the per-app instance cap above). Unknown plans fall
		// through the accessor to 0 — same fail-closed contract
		// as MaxMinInstances.
		ConcurrencyPerVMBound: plan.ConcurrencyPerVMBound(),
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
		// Issue #560: per-app require_authn flag. Surfaced so
		// dashboards can show "auth required on / off" alongside
		// the streaming + require_signed pills, and so a customer
		// can verify their PATCH landed without a second
		// round-trip. The token-scope enforcement (cross-account
		// 403) lives in gatewayd-internal, not here.
		RequireAuthn: a.RequireAuthn,
		// Issue #477 / ADR-079: per-app public-URL auth.
		// Surfaced so dashboards can show "public auth: open /
		// bearer / basic" alongside the require_authn pill and
		// so a customer can verify their PATCH landed without
		// a second round-trip. The plaintext creds NEVER
		// appear here — they live in app_secrets (ADR-045).
		PublicAuth: api.PublicAuthStatus{
			Mode:          a.PublicAuthMode,
			HasBasicCreds: len(a.PublicAuthBasicSealed) > 0,
		},
		// Issue #695 / ADR-080: grand-father marker. Set by
		// migration 00155 on every pre-flip row; null on
		// apps created after the flip. Surfaced so the
		// dashboard banner query + `faas apps list`
		// annotation can render the "since YYYY-MM-DD"
		// suffix on grandfathered rows.
		AuthDefaultFlippedAt: a.AuthDefaultFlippedAt,
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
		// Issue #470 / ADR-055: per-app two-tier-snapshot flag +
		// thresholds. Surfaced so dashboards can show "warm snapshot
		// on / off" alongside the streaming + require_signed pills,
		// and so a customer can verify the per-app override values
		// they PATCHed.
		WarmSnapshotEnabled:     a.WarmSnapshotEnabled,
		WarmSnapshotMinRequests: a.WarmSnapshotMinRequests,
		WarmSnapshotMinMs:       a.WarmSnapshotMinMs,
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

// emitAppCreated fires the Phase 2 / Gate A placement-claim notify
// that schedd's PlacementClaimSubscriber consumes (migration 00084 +
// ADR-055). The subscriber filters by payload.kind=="created", reads
// apps.row.node_id == NULL (newly inserted), and runs
// Engine.ClaimUnplaced to stamp the owner. The conditional UPDATE in
// Store.SetAppNodeID serialises N schedds into exactly one winner;
// losers drop silently.
//
// Failure here is best-effort: the cold-start sweep at schedd boot
// (cmd/schedd/main.go's ListUnplacedApps + ClaimUnplaced pass)
// reconciles an unplaced app on next restart, so a transient
// Postgres notify outage is bounded to "reboot picks it up" rather
// than "app never gets owned".
//
// Both fields are server-validated before persist (validSlug regex,
// server-generated app.ID UUID), so the JSON interpolation is safe
// even without explicit escaping — the team's pattern, mirrored from
// the existing updateApp emit (handlers_ext.go).
func (s *server) emitAppCreated(ctx context.Context, created state.App) {
	if s.notif == nil {
		return
	}
	// codeql[go/log-injection] false-positive: created.Slug passes
	// validSlug's regex (^[a-z0-9]([a-z0-9-]{1,38})[a-z0-9]$) at
	// buildApp() before INSERT; created.ID is a server-generated
	// UUID. The interpolation cannot reach a control character
	// or quote. Suppression placed at column 1 per the team's
	// pattern (memory: codeql-suppression-column1).
	_ = s.notif.Notify(ctx, db.NotifyAppChanged,
		fmt.Sprintf(`{"kind":"created","slug":"%s","app_id":"%s"}`, created.Slug, created.ID))
}
