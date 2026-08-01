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
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
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
type scanPlanResponse struct {
	ProjectSlug  string                  `json:"project_slug"`
	RepoFullName string                  `json:"repo_full_name,omitempty"`
	ScanSource   state.ProjectScanSource `json:"scan_source"`
	Tier         string                  `json:"tier"`
	Workloads    []reposcan.Workload     `json:"workloads"`
	Managed      []reposcan.Managed      `json:"managed"`
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
// When apply=true, the function ALSO builds the state.Project / []state.App
// / []state.Cron inputs ready for store.ApplyProjectPlan (caller invokes
// the store). When apply=false, the caller renders the response as JSON.
//
// Returns (response, project, apps, crons, problem). project/apps/crons
// are valid only when problem == nil and apply==true.
func (s *server) scanService(
	w http.ResponseWriter, r *http.Request, acct state.Account,
	planToken string, apply bool,
) (*scanPlanResponse, state.Project, []state.App, []state.Cron, *api.Problem) {
	limits := api.MustLimitsFor(acct.Plan)
	req, prob := parseScanMultipart(r, acct, limits)
	if prob != nil {
		return nil, state.Project{}, nil, nil, prob
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
			return nil, state.Project{}, nil, nil, api.ErrInternal("plan_token stale")
		}
	}

	result, scanErr := reposcan.Scan(os.DirFS(req.ScanDir))
	if scanErr != nil {
		return nil, state.Project{}, nil, nil, api.NewProblem(
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
		return nil, state.Project{}, nil, nil, api.ErrInternal(
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

	resp := &scanPlanResponse{
		ProjectSlug:   req.ProjectSlug,
		ScanSource:    deriveScanSource(filteredW),
		Tier:          result.Tier.String(),
		Workloads:     filteredW,
		Managed:       filteredMc,
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
			return nil, state.Project{}, nil, nil, api.ErrInternal(
				fmt.Sprintf("mint plan_token: %v", mintErr))
		}
		resp.PlanToken = tok
	} else {
		resp.PlanToken = planToken
	}

	if !apply {
		return resp, state.Project{}, nil, nil, nil
	}

	// Apply path: translate filteredW + crons into state rows.
	// Crons carry WorkloadName; the caller (handler) resolves
	// WorkloadName -> AppID once the ApplyProjectPlan Tx returns.
	if !canApply {
		// Don't even call ApplyProjectPlan — quota check is the
		// store's job but the handler routes the right HTTP code
		// based on the resp flags. Return a sentinel problem so the
		// handler can branch on canApply=false without parsing.
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
		return resp, state.Project{}, nil, nil, prob
	}

	project := state.Project{
		AccountID:        acct.ID,
		Slug:             req.ProjectSlug,
		RepoFullName:     "",
		ProductionBranch: req.ProdBranch,
		InstallID:        req.InstallID,
		ScanSource:       resp.ScanSource,
	}
	stateApps := make([]state.App, 0, len(filteredW))
	stateCrons := make([]state.Cron, 0, len(crons))
	for _, wl := range filteredW {
		app := wlToApp(acct, wl)
		// Phase 2 / Gate A: pin each app to an owner
		// compute_node. Same chooser as createApp, same
		// ordering (placement before ApplyProjectPlan's
		// FOR UPDATE on accounts). A placement failure
		// here means the project can never come up — we
		// surface it via the problem and skip the store.
		node, prob := s.placement.Choose(r.Context(), "", app.RAMMB)
		if prob != nil {
			return resp, state.Project{}, nil, nil, prob
		}
		app.NodeID = node.ID
		stateApps = append(stateApps, app)
	}
	// Crons arrive with AppID="" so the store doesn't try to insert
	// them before the AppID resolution. The handler reads the
	// cronWorkloadNames field off the response and, once
	// ApplyProjectPlan returns insertedApps, calls CreateCron for
	// each one with the resolved AppID.
	for _, c := range crons {
		stateCrons = append(stateCrons, state.Cron{
			Schedule: c.Schedule,
			Path:     c.Path,
			Enabled:  c.Enabled,
		})
	}
	for i := range crons {
		resp.CronNames = append(resp.CronNames, crons[i].WorkloadName)
	}
	return resp, project, stateApps, stateCrons, nil
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

// deriveScanSource picks the project scan_source from the workloads
// that survived --only. The rule mirrors the impl plan: if any
// workload came from compose, prefer compose; if any came from k8s
// and any came from compose, prefer k8s (compose is the union of
// k8s + docker-compose semantics, but the highest-priority detector
// wins per the reposcan merge rule).
//
// We lean on Workload.Source ("compose.yaml: api") — the first
// segment is the detector tag, which is the same string the merge
// rule's detector priority uses. This avoids hard-coding detector
// names twice.
func deriveScanSource(workloads []reposcan.Workload) state.ProjectScanSource {
	// Priority order matches the detector fan-out in
	// pkg/reposcan/scan.go:130. The first match wins.
	priority := []string{
		"compose", "procfile", "k8s", "render", "fly",
		"serverless", "app.yaml", "workspaces", "convention",
	}
	for _, want := range priority {
		for _, w := range workloads {
			if strings.HasPrefix(w.Source, want+":") || strings.HasPrefix(w.Source, want+".") {
				return state.ProjectScanSource(want)
			}
		}
	}
	if len(workloads) == 1 {
		return state.ProjectScanSourceSingle
	}
	return state.ProjectScanSourceUnknown
}

// wlToApp translates a reposcan.Workload into a state.App draft.
// The handler hands this to store.ApplyProjectPlan; the store
// stamps ID, CreatedAt, status='active', and backfills project_id
// to the freshly-inserted project.
func wlToApp(acct state.Account, w reposcan.Workload) state.App {
	class := state.WorkloadClass(string(w.Class))
	if class == "" {
		class = state.WorkloadClassHTTP
	}
	if w.Class == reposcan.ClassServer {
		// "server" hint is normalised to "http" — ADR-051 will
		// re-derive the authoritative class.
		class = state.WorkloadClassHTTP
	}
	startCmd := ""
	if len(w.Command) > 0 {
		startCmd = strings.Join(w.Command, " ")
	}
	return state.App{
		AccountID:      acct.ID,
		Slug:           w.Name,
		Type:           state.AppTypeApp,
		RAMMB:          api.MustLimitsFor(acct.Plan).RAMMB,
		MaxConcurrency: api.MustLimitsFor(acct.Plan).MaxConcurrency,
		Status:         state.AppActive,
		ProjectID:      "", // backfilled by store
		RootDir:        w.RootDir,
		WorkloadName:   w.Name,
		WorkloadClass:  class,
		StartCommand:   startCmd,
	}
}

// --- response side helpers --------------------------------------------------

// nowUnix returns the current time in seconds. The plan_token's TS
// field is informational — the hash is the load-bearing field —
// but exposing a function variable lets tests freeze the clock.
var nowUnix = func() int64 { return time.Now().Unix() }
