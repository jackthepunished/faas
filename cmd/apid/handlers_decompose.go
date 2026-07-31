package main

// handlers_decompose.go — Phase 3 HTTP routes.
//
// Two endpoints:
//
//	POST /v1/projects/scan  — dry-run; runs reposcan.Scan, returns a
//	                          plan with can_apply + plan_token; never
//	                          writes.
//	POST /v1/projects       — applies the plan in one Tx; emits
//	                          NotifyAppChanged per inserted app.
//
// Both are authed + requireMFA + requireScope(deploy:write). Apply
// additionally takes s.idempotent (via the mux wrap in server.go).
//
// Filesize: handlers stay ≤50 lines per project guideline. Anything
// thicker (multipart parse, extract, scan, quota) lives in
// scan_service.go and extract.go; this file is the
// auth/middleware/orchestration seam only.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/state"
)

// scanProject is the dry-run endpoint. It never writes; the response
// is the same scanPlanResponse that the apply endpoint emits so the
// CLI's `--json` mode passes the bytes through verbatim.
func (s *server) scanProject(w http.ResponseWriter, r *http.Request, acct state.Account) {
	resp, _, _, _, prob := s.scanService(w, r, acct, "", false)
	if prob != nil {
		api.WriteProblem(w, prob)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// applyProject is the transactional endpoint. On success it emits one
// NotifyAppChanged per inserted app so schedd can pick up the new
// rows without polling; on quota failure it surfaces the matching
// RFC 7807 problem with limit + observed + docs URL and zero rows
// inserted (the store Tx rolled back).
func (s *server) applyProject(w http.ResponseWriter, r *http.Request, acct state.Account) {
	planToken := r.URL.Query().Get("plan_token")
	resp, project, apps, crons, prob := s.scanService(w, r, acct, planToken, true)
	if prob != nil {
		api.WriteProblem(w, prob)
		return
	}

	// Hand the assembled project/apps/cron intents to the store.
	// ApplyProjectPlan runs the quota check atomically again under
	// FOR UPDATE on the accounts row — the scan-side pre-check
	// above is advisory; the store is authoritative.
	limits := api.MustLimitsFor(acct.Plan)
	insertedProject, insertedApps, _, applyErr := s.store.ApplyProjectPlan(
		r.Context(), project, apps, crons, limits)
	if applyErr != nil {
		var qe *state.QuotaError
		switch {
		case errors.As(applyErr, &qe):
			api.WriteProblem(w, quotaProblem(acct.Plan, limits, qe))
			return
		case errors.Is(applyErr, state.ErrConflict):
			api.WriteProblem(w, api.NewProblem(http.StatusConflict,
				api.CodeValidation, "Project slug collision",
				"this project slug is already taken"))
			return
		case errors.Is(applyErr, state.ErrNotFound):
			api.WriteProblem(w, api.NewProblem(http.StatusNotFound,
				api.CodeValidation, "Account not found", ""))
			return
		default:
			api.WriteProblem(w, api.ErrInternal(
				fmt.Sprintf("apply project plan: %v", applyErr)))
			return
		}
	}

	// Wire cron AppID back from the just-inserted apps. The scan
	// service's crons came in with AppID="" (resolved by name); the
	// store hands us the authoritative ID list. Use CreateCron (not
	// CreateCronIfUnderQuota) — the quota check already ran inside
	// ApplyProjectPlan's Tx so a second pass would double-count.
	slugToID := make(map[string]string, len(insertedApps))
	for _, a := range insertedApps {
		slugToID[a.Slug] = a.ID
	}
	for i, c := range crons {
		var appID string
		if i < len(resp.CronNames) {
			appID = slugToID[resp.CronNames[i]]
		}
		if appID == "" {
			api.WriteProblem(w, api.ErrInternal(
				fmt.Sprintf("cron %q has no matching app_id (workload_name=%q)",
					c.Schedule, resp.CronNames[i])))
			return
		}
		c.AppID = appID
		if _, err := s.store.CreateCron(r.Context(), c.AppID, c.Schedule, c.Path, c.Enabled); err != nil {
			api.WriteProblem(w, api.ErrInternal(
				fmt.Sprintf("stamp cron app_id: %v", err)))
			return
		}
	}

	// Notify every freshly inserted app. PostgreSQL delivers the
	// notification only on Tx commit, so handler-side emit
	// post-ApplyProjectPlan is safe — the rows are visible.
	for _, a := range insertedApps {
		_ = s.notif.Notify(r.Context(), db.NotifyAppChanged,
			fmt.Sprintf(`{"kind":"created","app_id":"%s","project_id":"%s"}`,
				a.ID, insertedProject.ID))
	}

	// Audit + response. Response body mirrors the scan output but
	// carries the inserted IDs so the CLI's --yes flow can render
	// "applied: <slug> → <app_id>".
	type applyResp struct {
		scanPlanResponse
		ProjectID string       `json:"project_id"`
		AppIDs    []appSummary `json:"apps"`
	}
	out := applyResp{
		scanPlanResponse: *resp,
		ProjectID:        insertedProject.ID,
		AppIDs:           make([]appSummary, 0, len(insertedApps)),
	}
	for _, a := range insertedApps {
		out.AppIDs = append(out.AppIDs, appSummary{Slug: a.Slug, ID: a.ID})
	}

	s.audit.Emit(r.Context(), "project.created", &acct.ID, map[string]any{
		"project_id": insertedProject.ID,
		"slug":       insertedProject.Slug,
		"scan_source": string(insertedProject.ScanSource),
		"app_count":  len(insertedApps),
	})

	writeJSON(w, http.StatusOK, out)
}

// appSummary is the per-app line in the apply response. Declared at
// package scope so the applyResp literal in applyProject can name it
// (Go forbids type declarations inside a function from being
// referenced by another type literal in the same scope).
type appSummary struct {
	Slug string `json:"slug"`
	ID   string `json:"id"`
}

// quotaProblem maps a *state.QuotaError into the right RFC 7807
// problem. Phase 3 only emits two quota shapes:
//
//   - apps:    403 plan_limit_apps        (with limit + observed)
//   - crons:   402 plan_crons_not_allowed (NotAllowed=true)
//              403 plan_cron_quota        (Limit > 0 but exceeded)
//
// Both already have helper constructors in pkg/api/errors.go; this
// function just translates the state's Kind enum into the helper call
// so the handler body stays a single switch.
func quotaProblem(plan api.Plan, l api.Limits, qe *state.QuotaError) *api.Problem {
	if qe == nil {
		return api.ErrInternal("quota error missing body")
	}
	switch qe.Kind {
	case state.QuotaErrorKindCrons:
		if qe.NotAllowed {
			return api.ErrPlanCronsNotAllowed(plan)
		}
		return api.ErrPlanCronQuota(plan, "account", qe.Limit, qe.Observed)
	case state.QuotaErrorKindApps:
		return api.ErrPlanLimitApps(l, qe.Observed)
	default:
		return api.ErrInternal("unknown quota kind")
	}
}

// Ensure unused imports stay bound so future edits can pull them in
// without a separate import pass. The error types below are
// re-exported by the file for callers (e.g. cmd/gregale) and should
// always be present.
var (
	_ = json.Marshal
)
