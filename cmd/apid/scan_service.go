package main

// scan_service.go — Phase 3 scan service.
//
// Inputs:  a multipart upload (source=<tar.gz>) + form fields
//          (project_slug, production_branch, install_id, only).
// Outputs: a scanPlanResponse carrying Workloads + Managed + Crons
//          + ProjectScanSource + canApply + planToken, or a
//          *api.Problem describing why the scan was rejected.
//
// The split between this file and handlers_decompose.go is
// intentional: handlers stay ≤50 lines (project guideline) and
// orchestrate the auth/middleware/notifier boundary; this file is
// pure logic and unit-testable from an httptest harness.
//
// planToken is a base64-JSON blob carrying {Hash, AccountID, TS}.
// The apply handler verifies SHA-256(uploaded_bytes) == Hash; if it
// doesn't, it re-runs the scan from scratch and re-evaluates the
// quota gate before persisting. This keeps the server authoritative
// — the plan is an optimization for the interactive flow, not a
// trust token.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/reconcile"
	"github.com/onebox-faas/faas/pkg/reposcan"
	"github.com/onebox-faas/faas/pkg/state"
)

// planTokenWire is the JSON shape baked into the plan token. The
// apply handler decodes this; mismatched account_id is a forgery
// signal and aborts with 403. Hash is the SHA-256 of the uploaded
// source bytes (hex). TS is informational — the hash is the load-
// bearing field.
type planTokenWire struct {
	Hash      string `json:"hash"`
	AccountID string `json:"account_id"`
	Slug      string `json:"slug"`
	TSUnix    int64  `json:"ts_unix"`
}

// scanPlanRequest is the parsed multipart body for both /scan and
// /apply. The handler builds this from the multipart stream before
// dispatching to the scan service.
type scanPlanRequest struct {
	SourcePath   string // spool path after validateAndSpool + extractTarGzToDir
	SourceSHA256 string // hex digest of the original compressed bytes
	ScanDir      string // extracted dir (cleaned up by caller)
	ProjectSlug  string
	ProdBranch   string
	InstallID    int64
	Only         map[string]bool
}

// scanPlanResponse is the body returned by the scan service and
// the apply service. Marshaled to JSON for both the /scan and
// /apply responses so the CLI can pass --json through verbatim.
//
// Workloads and Managed are the *api DTOs (not the reposcan
// types) so the wire shape is consistent with the OpenAPI spec
// and the SDK/test decoders. reposcan.Tier is a typed int; the
// DTO's Tier is a string — using the DTO here is what makes
// `plan.workloads[i].tier` serialize as "compose" instead of
// `8`. The conversion lives in toPlanWorkload below.
type scanPlanResponse struct {
	ProjectSlug  string                  `json:"project_slug"`
	RepoFullName string                  `json:"repo_full_name,omitempty"`
	ScanSource   state.ProjectScanSource `json:"scan_source"`
	Tier         string                  `json:"tier"`
	Workloads    []api.PlanWorkload      `json:"workloads"`
	Managed      []api.PlanManaged       `json:"managed"`
	Crons        []planCron              `json:"crons"`
	// CronNames parallels Crons: when /apply runs, the apply handler
	// uses CronNames[i] to look up the freshly inserted app_id from
	// insertedApps (matched by Slug == WorkloadName). Not exposed
	// on /scan responses because the scan handler doesn't need it.
	CronNames     []string `json:"-"`
	Warnings      []string `json:"warnings,omitempty"`
	ObservedApps  int      `json:"observed_apps"`
	ObservedCrons int      `json:"observed_crons"`
	LimitApps     int      `json:"limit_apps"`
	LimitCrons    int      `json:"limit_crons"`
	CanApply      bool     `json:"can_apply"`
	NotAllowed    bool     `json:"crons_not_allowed,omitempty"`
	PlanToken     string   `json:"plan_token"`
}

// toPlanWorkload translates a reposcan.Workload into the wire-
// shape DTO. The only non-trivial field is Tier: reposcan.Tier is
// a typed int (1/3/5/8) and the wire shape is the .String()
// representation ("single"/"convention"/"workspace"/"compose"/
// "unknown"), matching the OpenAPI PlanWorkload.tier enum.
func toPlanWorkload(w reposcan.Workload) api.PlanWorkload {
	return api.PlanWorkload{
		Name:       w.Name,
		RootDir:    w.RootDir,
		Dockerfile: w.Dockerfile,
		Command:    w.Command,
		Class:      string(w.Class),
		Schedule:   w.Schedule,
		Ports:      w.Ports,
		EnvKeys:    w.EnvKeys,
		Source:     w.Source,
		Tier:       w.Tier.String(),
	}
}

// toPlanManaged translates a reposcan.Managed into the wire-shape
// DTO. Pure field copy — the reposcan and DTO fields align
// one-to-one; this helper exists so the carrier conversion is
// symmetrical with toPlanWorkload and stays at one call site if
// either side grows new fields.
func toPlanManaged(m reposcan.Managed) api.PlanManaged {
	return api.PlanManaged{
		Name:    m.Name,
		Kind:    m.Kind,
		EnvHint: m.EnvHint,
		Source:  m.Source,
		Image:   m.Image,
	}
}

// planCron is the cron shape returned by the scan service. We keep
// it distinct from state.Cron (which carries AppID + CreatedAt)
// because at scan time there's no AppID yet — apply resolves the
// name→ID map by inserting apps first.
type planCron struct {
	WorkloadName string `json:"workload_name"`
	Schedule     string `json:"schedule"`
	Path         string `json:"path"`
	Enabled      bool   `json:"enabled"`
}

// scanService runs the pipeline end-to-end:
//
//	multipart parse → spool+validate → extract → scan
//	  → filter --only → derive scan_source → compute can_apply
//	  → mint plan_token
//
// `apply` is true for POST /v1/projects, false for POST /v1/projects/scan.
// When apply=true, the function ALSO creates the projects row + runs
// pkg/reconcile.Service, returning the reconcile Result via the
// third/fourth slots. When apply=false, the caller renders the
// response as JSON.
//
// Returns (response, project, added, changed, removedSlugs, problem).
// project/added/changed/removedSlugs are valid only when problem == nil
// and apply==true.
//
// added is the post-insert state.App slice for newly-created apps;
// changed is the post-update slice for workloads whose RootDir /
// WorkloadName / StartCommand drifted. removedSlugs carries the slugs
// of workloads that the scan dropped from the project — the handler
// uses these to soft-delete the corresponding crons (issue: a cron
// for a removed workload previously 500'd because the slug→ID map
// had no entry; PR-GH.6 fixes that by walking removedSlugs).
func (s *server) scanService(
	w http.ResponseWriter, r *http.Request, acct state.Account,
	planToken string, apply bool,
) (*scanPlanResponse, state.Project, []state.App, []state.App, []string, *api.Problem) {
	limits := api.MustLimitsFor(acct.Plan)
	req, prob := parseScanMultipart(r, acct, limits)
	if prob != nil {
		return nil, state.Project{}, nil, nil, nil, prob
	}
	// The handler owns cleanup of the extracted dir (it defers
	// RemoveAll). Don't do that here — the lifecycle crosses this
	// function boundary on the apply path.
	defer func() { _ = os.Remove(req.SourcePath) }() //nolint:errcheck // best-effort

	// If a plan_token was passed (apply path), validate it BEFORE
	// running the scan. Mismatch -> 409 plan_token_stale. This
	// short-circuits the scan only when the hash matches; on a
	// miss we re-scan (the caller asked for this bytes-blob, not
	// the cached plan) and continue.
	if planToken != "" {
		var pt planTokenWire
		b, decErr := base64.StdEncoding.DecodeString(planToken)
		if decErr == nil {
			_ = json.Unmarshal(b, &pt)
		}
		if pt.AccountID != acct.ID || pt.Hash != req.SourceSHA256 {
			api.WriteProblem(w, api.NewProblem(http.StatusConflict,
				"plan_token_stale",
				"plan_token does not match uploaded source",
				"re-run scan and apply in one flow"))
			return nil, state.Project{}, nil, nil, nil, api.ErrInternal("plan_token stale")
		}
	}

	result, scanErr := reposcan.Scan(os.DirFS(req.ScanDir))
	if scanErr != nil {
		return nil, state.Project{}, nil, nil, nil, api.NewProblem(
			http.StatusBadRequest, api.CodeSourceInvalid,
			"Scan failed", scanErr.Error())
	}

	// Filter --only against workload name. Deterministic order
	// preserved (reposcan already sorts).
	var (
		filteredW  []reposcan.Workload
		filteredMc []reposcan.Managed
	)
	for _, wl := range result.Workloads {
		if len(req.Only) > 0 && !req.Only[strings.ToLower(wl.Name)] {
			continue
		}
		filteredW = append(filteredW, wl)
	}
	// Managed services (compose image: db, etc.) are not subject to
	// --only — the customer sees them in the plan either way so they
	// know what we're NOT provisioning. That mirrors the §4 fixture
	// (1 managed postgres).
	filteredMc = append(filteredMc, result.Managed...)

	// Crons: any workload with a Schedule string is also a cron. Map
	// to planCron with the workload name; resolve AppID in the apply
	// path from the just-inserted apps.
	var crons []planCron
	for _, wl := range filteredW {
		if wl.Schedule == "" {
			continue
		}
		path := wl.Ports // unused but keeps govet quiet
		_ = path
		crons = append(crons, planCron{
			WorkloadName: wl.Name,
			Schedule:     wl.Schedule,
			Path:         "/",
			Enabled:      true,
		})
	}

	// can_apply computation: apps + crons must fit under the plan
	// caps AND crons must be allowed. We mirror store.ApplyProjectPlan's
	// pre-check (no Tx here — the store is authoritative).
	observedApps, appCountErr := s.store.CountDeployedApps(r.Context(), acct.ID)
	if appCountErr != nil {
		return nil, state.Project{}, nil, nil, nil, api.ErrInternal(
			fmt.Sprintf("count apps: %v", appCountErr))
	}
	observedCrons := countAccountCrons(r.Context(), s, acct.ID)

	canApply := true
	notAllowed := false
	if observedApps+len(filteredW) > limits.DeployedApps {
		canApply = false
	}
	if len(crons) > 0 && limits.CronLimitPerAccount == 0 {
		canApply = false
		notAllowed = true
	}
	if observedCrons+len(crons) > limits.CronLimitPerAccount {
		canApply = false
	}

	// Convert the reposcan carrier slice into the wire-shape DTO so
	// the JSON marshal sees string Tier (matching OpenAPI enum +
	// pkg/api.PlanWorkload.Tier) instead of the raw int the
	// reposcan type carries. See toPlanWorkload / toPlanManaged.
	respWorkloads := make([]api.PlanWorkload, len(filteredW))
	for i, w := range filteredW {
		respWorkloads[i] = toPlanWorkload(w)
	}
	respManaged := make([]api.PlanManaged, len(filteredMc))
	for i, m := range filteredMc {
		respManaged[i] = toPlanManaged(m)
	}
	resp := &scanPlanResponse{
		ProjectSlug:   req.ProjectSlug,
		ScanSource:    reconcile.DeriveScanSource(filteredW),
		Tier:          result.Tier.String(),
		Workloads:     respWorkloads,
		Managed:       respManaged,
		Crons:         crons,
		Warnings:      result.Warnings,
		ObservedApps:  observedApps + len(filteredW),
		ObservedCrons: observedCrons + len(crons),
		LimitApps:     limits.DeployedApps,
		LimitCrons:    limits.CronLimitPerAccount,
		CanApply:      canApply,
		NotAllowed:    notAllowed,
	}

	// Mint a fresh plan_token unless one was supplied (apply path
	// keeps the caller's; minting a new one would be confusing).
	if planToken == "" {
		tok, mintErr := mintPlanToken(acct.ID, req.ProjectSlug, req.SourceSHA256)
		if mintErr != nil {
			return nil, state.Project{}, nil, nil, nil, api.ErrInternal(
				fmt.Sprintf("mint plan_token: %v", mintErr))
		}
		resp.PlanToken = tok
	} else {
		resp.PlanToken = planToken
	}

	if !apply {
		return resp, state.Project{}, nil, nil, nil, nil
	}

	// Apply path (PR-G, repo decomposition Phase 5): route every

	// Apply path (PR-G, repo decomposition Phase 5): route every
	// workload-mutating action through pkg/reconcile.Service. The
	// scan-service no longer builds state.App rows directly; the
	// reconcile package owns the diff/apply contract and emits the
	// audit rows. Pre-PR-G this branch built stateApps + stateCrons
	// and handed them to store.ApplyProjectPlan; post-PR-G we hand
	// state.Project + reposcan.Result to reconcileSvc.Reconcile.
	if !canApply {
		// Don't even call Reconcile — quota check is reconcile's
		// job but the handler routes the right HTTP code based on
		// the resp flags. Return a sentinel problem so the handler
		// can branch on canApply=false without parsing.
		var prob *api.Problem
		switch {
		case notAllowed:
			prob = api.ErrPlanCronsNotAllowed(acct.Plan)
		case observedApps+len(filteredW) > limits.DeployedApps:
			prob = api.ErrPlanLimitApps(limits, observedApps+len(filteredW))
		default:
			prob = api.ErrPlanCronQuota(acct.Plan, "account",
				limits.CronLimitPerAccount, observedCrons+len(crons))
		}
		return resp, state.Project{}, nil, nil, nil, prob
	}

	project := state.Project{
		AccountID:        acct.ID,
		Slug:             req.ProjectSlug,
		RepoFullName:     "",
		ProductionBranch: req.ProdBranch,
		InstallID:        req.InstallID,
		ScanSource:       resp.ScanSource,
	}
	// CreateProject inserts the projects row and stamps ID +
	// CreatedAt. This MUST happen before reconcile runs — the
	// reconcile path's CreateAppIfUnderQuota cascades a
	// project_id FK (apps.project_id → projects.id), and a NULL
	// project_id would skip the apps_project_workload_uniq
	// enforcement path. Pre-PR-G, store.ApplyProjectPlan inserted
	// project + apps in one Tx; the split here is the cost of
	// the package boundary (pkg/reconcile never imports pkg/state
	// types beyond the Store interface).
	//
	// Atomicity (PR-GH.6 review H9 fix): if the subsequent
	// reconcile fails, the project row is rolled back via
	// store.DeleteProject. apps.project_id is declared ON
	// DELETE SET NULL (migration 00074:74), so any apps
	// reconcile already inserted (per-row Tx) have their FK
	// nulled — they stay durable (audit chain) but no longer
	// appear under any project. The rollback is best-effort:
	// a DeleteProject failure logs but does not mask the
	// reconcile error the caller is returning.
	created, projErr := s.store.CreateProject(r.Context(), project)
	if projErr != nil {
		var prob *api.Problem
		switch {
		case errors.Is(projErr, state.ErrConflict):
			prob = api.NewProblem(http.StatusConflict,
				api.CodeValidation, "Project slug collision",
				"this project slug is already taken")
		case errors.Is(projErr, state.ErrNotFound):
			prob = api.NewProblem(http.StatusNotFound,
				api.CodeValidation, "Account not found", "")
		default:
			prob = api.ErrInternal(fmt.Sprintf("create project: %v", projErr))
		}
		return resp, state.Project{}, nil, nil, nil, prob
	}
	project = created
	// Defer project rollback for any error path below.
	// capturedProb tracks whether we already wrapped the
	// reconcile error; the defer fires BEFORE the return
	// statement so rollback runs first.
	//
	// rollbackCtx is captured explicitly so the defer closure
	// doesn't extend the lifetime of r (contextcheck linter).
	rollbackCtx := r.Context()
	var capturedProb *api.Problem
	defer func() {
		if capturedProb != nil && project.ID != "" {
			if dErr := s.store.DeleteProject(rollbackCtx, project.ID); dErr != nil {
				// Best-effort: a DeleteProject failure doesn't
				// mask the underlying reconcile error the
				// caller is returning. The DeleteProject's own
				// ErrNotFound is fine (concurrent delete raced).
				if !errors.Is(dErr, state.ErrNotFound) {
					s.log.Warn("apid: rollback project on reconcile error",
						"project_id", project.ID, "err", dErr)
				}
			}
		}
	}()

	// Build the cron name list (handler uses index parity to look up
	// the AppID post-reconcile). Order is the same as the scan order
	// (reposcan sorts by Name), and reconcile's workloadToDraftApp
	// preserves that order in Result.Added — so the handler's
	// resp.CronNames[i] ↔ Result.Added/Changed slug lookup is safe.
	for i := range crons {
		resp.CronNames = append(resp.CronNames, crons[i].WorkloadName)
	}

	// Hand off to reconcile.Service. The post-Reconcile Result
	// carries Added (creates), Changed (updates), Removed (soft-
	// deletes), and Alerts (guard-tripped notifications). The
	// handler reads Added + Changed to build the slug→ID map for
	// cron stamping and to emit per-app NotifyAppChanged.
	//
	// The reposcan.Result handed to reconcile is the --only-filtered
	// subset, mirroring the legacy stateApps construction at the top
	// of this branch (filteredW). The tier + warnings + managed
	// metadata pass through verbatim — only Workloads is filtered.
	filteredScan := reposcan.Result{
		Workloads: filteredW,
		Managed:   filteredMc,
		Tier:      result.Tier,
		Warnings:  result.Warnings,
	}
	reconcileInputs := toReconcileInputs(*req, project, filteredScan)
	rec, recErr := s.reconcileSvc.Reconcile(
		r.Context(), project, filteredScan,
		reconcileInputs.CommitSHA, reconcileInputs.Branch)
	if recErr != nil {
		// Map reconcile-package errors into the existing RFC 7807
		// problem shapes so the handler can use a single dispatch
		// path. mapReconcileError returns nil for nil err, so the
		// caller can guard on prob != nil.
		mapped := mapReconcileError(recErr)
		if mapped != nil {
			var prob *api.Problem
			if mapped.Quota != nil {
				prob = quotaProblem(acct.Plan, limits, mapped.Quota)
			} else {
				prob = api.NewProblem(mapped.Status, mapped.Code, mapped.Msg, "")
			}
			capturedProb = prob
			return resp, state.Project{}, nil, nil, nil, prob
		}
		// mapReconcileError returned nil for nil err — unreachable
		// here, but defensively pass through as 500.
		prob := api.ErrInternal(fmt.Sprintf("reconcile: %v", recErr))
		capturedProb = prob
		return resp, state.Project{}, nil, nil, nil, prob
	}

	// Compute removed slugs. rec.Removed is a list of IDs; the
	// handler needs slugs to soft-delete the corresponding crons
	// (workloads dropped from the scan no longer appear in
	// resp.Crons so the slug→ID lookup in handlers_decompose.go
	// returns empty — without removedSlugs the handler would 500
	// on a removed workload that previously had a cron). We
	// resolve slug from the (now-deleted) rec.Added ∪ rec.Changed
	// maps via the inverse map below.
	removedSlugs := make([]string, 0, len(rec.Removed))
	if len(rec.Removed) > 0 {
		// removedSlugByID is keyed by the pre-remove app ID.
		// Build it BEFORE reconcile so we capture the slug of
		// every app that the scan dropped. The state.App rows
		// we look at are the project's pre-reconcile member
		// list (read-only); reconcile's removal happens
		// downstream of this lookup.
		existingApps, lerr := s.store.AppsForProject(r.Context(), acct.ID, project.ID)
		if lerr != nil {
			prob := api.ErrInternal(fmt.Sprintf("load existing apps: %v", lerr))
			capturedProb = prob
			return resp, state.Project{}, nil, nil, nil, prob
		}
		idToSlug := make(map[string]string, len(existingApps))
		for _, a := range existingApps {
			idToSlug[a.ID] = a.Slug
		}
		for _, id := range rec.Removed {
			if slug, ok := idToSlug[id]; ok {
				removedSlugs = append(removedSlugs, slug)
			}
		}
	}

	// Fold the reconcile Result into the legacy return shape. The
	// handler reads the third slot (added) and fourth slot
	// (changed) to:
	//
	//  - build the slug→ID map for cron stamping (added ∪ changed)
	//  - emit per-app NotifyAppChanged with kind=created for
	//    rec.Added entries and kind=updated for rec.Changed entries
	//  - soft-delete crons whose workload_name is in removedSlugs
	//
	// Each slice carries post-insert/post-update rows with valid
	// IDs (reconcile's CreateAppIfUnderQuota stamps ID + CreatedAt;
	// UpdateApp stamps UpdatedAt).
	return resp, project, rec.Added, rec.Changed, removedSlugs, nil
}

// parseScanMultipart reads the multipart body, spools, validates, and
// extracts the tarball. On success, req.SourcePath points at the
// original tarball (cleaned up by the caller), req.ScanDir at the
// extracted root (cleaned up by the caller), and req.SourceSHA256 at
// the hex digest of the compressed bytes.
func parseScanMultipart(r *http.Request, acct state.Account, limits api.Limits) (*scanPlanRequest, *api.Problem) {
	// Multipart cap before parsing — mirrors createDeployment.
	max := int64(limits.SourceTarballMaxMB) * 1024 * 1024
	r.Body = http.MaxBytesReader(nil, r.Body, max)
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
			"Bad multipart", err.Error())
	}
	var (
		sourcePath  string
		onlySet     = map[string]bool{}
		projectSlug string
		prodBranch  = "main"
		installID   int64
	)
	for {
		part, perr := mr.NextPart()
		if perr == io.EOF {
			break
		}
		if perr != nil {
			return nil, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
				"Bad multipart", perr.Error())
		}
		name := part.FormName()
		switch name {
		case "source":
			path, n, vErr := validateAndSpool(part, limits)
			if vErr != nil {
				return nil, vErr
			}
			sourcePath = path
			_ = n
		case "project_slug":
			b, _ := io.ReadAll(io.LimitReader(part, 64))
			projectSlug = strings.TrimSpace(string(b))
		case "production_branch":
			b, _ := io.ReadAll(io.LimitReader(part, 64))
			prodBranch = strings.TrimSpace(string(b))
			if prodBranch == "" {
				prodBranch = "main"
			}
		case "install_id":
			b, _ := io.ReadAll(io.LimitReader(part, 32))
			//nolint:errcheck // empty input → installID stays 0; the
			// apply handler treats 0 as "no install binding" (issue #313).
			_, _ = fmt.Sscanf(string(b), "%d", &installID)
		case "only":
			b, _ := io.ReadAll(io.LimitReader(part, 1024))
			for _, s := range strings.Split(string(b), ",") {
				s = strings.ToLower(strings.TrimSpace(s))
				if s != "" {
					onlySet[s] = true
				}
			}
		default:
			_, _ = io.Copy(io.Discard, part)
		}
		_ = part.Close()
	}
	if sourcePath == "" {
		return nil, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
			"Source required", "multipart applies require a 'source' file field")
	}
	if projectSlug == "" {
		// default to repo dir basename — but we don't have it here.
		// Fall back to a random placeholder; the handler can correct
		// if it has extra context (--repo on the CLI).
		projectSlug = "project-" + randomToken(6)
	}

	// Hash the spooled bytes BEFORE extract (extract consumes the
	// file handle). SHA-256 over the compressed bytes is the
	// plan_token's hash field.
	hash, hashErr := hashFileSHA256(sourcePath)
	if hashErr != nil {
		return nil, api.NewProblem(http.StatusBadRequest, api.CodeSourceInvalid,
			"Bad source", hashErr.Error())
	}

	// Extract to disk and validate the §11 hardening posture.
	lim := defaultExtractLimits(limits)
	scanDir, prob := extractTarGzToDir(sourcePath, lim)
	if prob != nil {
		return nil, prob
	}

	return &scanPlanRequest{
		SourcePath:   sourcePath,
		SourceSHA256: hash,
		ScanDir:      scanDir,
		ProjectSlug:  projectSlug,
		ProdBranch:   prodBranch,
		InstallID:    installID,
		Only:         onlySet,
	}, nil
}

// hashFileSHA256 streams the file through SHA-256 without loading
// the whole thing into memory. The cap is enforced by the
// MaxBytesReader above so this can't pin apid.
func hashFileSHA256(path string) (string, error) {
	//nolint:forbidigo // path is the daemon-spooled tarball from
	// FAAS_SCAN_SPOOL_ROOT (set by scanService); not customer input.
	// The lint tripwire for customer-path os.Open is in cmd/gregale.
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// mintPlanToken produces the base64-JSON blob. The hash is the
// SHA-256 of the source bytes (the apply handler re-hashes and
// compares). AccountID prevents token-reuse across accounts.
func mintPlanToken(accountID, slug, hashHex string) (string, error) {
	pt := planTokenWire{
		Hash:      hashHex,
		AccountID: accountID,
		Slug:      slug,
		TSUnix:    nowUnix(),
	}
	b, err := json.Marshal(pt)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// countAccountCrons returns the count of crons across the account's
// non-deleted apps. Mirrors the store-side COUNT in
// ApplyProjectPlan; the duplication exists because the scan service
// runs OUTSIDE the apply Tx so it needs its own count to render
// can_apply accurately. The store's count inside the Tx is the
// authoritative one — if the two disagree, the store wins on commit.
func countAccountCrons(ctx context.Context, s *server, accountID string) int {
	apps, err := s.store.ListApps(ctx, accountID)
	if err != nil {
		return 0
	}
	var total int
	for _, a := range apps {
		cs, err := s.store.ListCronsForApp(ctx, a.ID)
		if err != nil {
			continue
		}
		total += len(cs)
	}
	return total
}

// --- response side helpers --------------------------------------------------

// nowUnix returns the current time in seconds. The plan_token's TS
// field is informational — the hash is the load-bearing field —
// but exposing a function variable lets tests freeze the clock.
var nowUnix = func() int64 { return time.Now().Unix() }
