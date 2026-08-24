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
	resp, _, _, _, _, _, prob := s.scanService(w, r, acct, "", false)
	if prob != nil {
		api.WriteProblem(w, prob)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// applyProject is the transactional endpoint. On success it emits one
// NotifyAppChanged per inserted/updated app so schedd can pick up
// the rows without polling; on quota failure it surfaces the matching
// RFC 7807 problem with limit + observed + docs URL and zero rows
// inserted (the store Tx rolled back).
//
// PR-G (repo decomposition Phase 5): the workload-mutation path
// moved from store.ApplyProjectPlan to pkg/reconcile.Service. The
// scanService now creates the projects row + reconciles apps under
// one logical flow. This handler is responsible for:
//
//  1. Reading the reconcile Result's added + changed slices from
//     scanService's third + fourth return values.
//  2. Stamping cron app_id from the post-reconcile slug→ID map.
//  3. Emitting per-app NotifyAppChanged (kind=created for added,
//     kind=updated for changed) so schedd picks up the rows.
//  4. Emitting the project.created audit row (gated by the
//     CreateProject call inside scanService — duplicate slugs are
//     rejected with ErrConflict before reconcile runs).
//  5. Rendering per-workload appliedBuild results in the response
//     so the CLI's `faas apply` flow can show "app X: deployment Y,
//     build Z" per workload (PR-A, Phase 5 close-the-loop).
func (s *server) applyProject(w http.ResponseWriter, r *http.Request, acct state.Account) {
	planToken := r.URL.Query().Get("plan_token")
	resp, insertedProject, added, changed, removedSlugs, builds, prob := s.scanService(w, r, acct, planToken, true)
	if prob != nil {
		api.WriteProblem(w, prob)
		return
	}

	// The reconcile path emits its own per-action audit rows
	// (project.workload.added / project.workload.changed); the
	// apid-side audit pipeline only fires the per-project
	// project.created row (at the bottom of this function).

	// Soft-delete crons for workloads the scan dropped (H8 fix).
	// Pre-fix, the cron-stamping loop below tried to look up
	// appID by slug for every cron in resp.CronNames; a workload
	// that was removed by the scan has no entry in the Added ∪
	// Changed slug map, and the handler 500'd. Today the loop
	// only touches crons in the NEW plan; orphans (a cron for a
	// removed workload) are soft-deleted here so the cron list
	// stays in sync with the workload list.
	for _, slug := range removedSlugs {
		// Find the app_id for the removed workload. The app
		// was just soft-deleted by reconcile, so its row is
		// still readable (PR-E sets status=AppDeleted).
		apps, lerr := s.store.AppsForProject(r.Context(), acct.ID, insertedProject.ID)
		if lerr != nil {
			s.audit.Emit(r.Context(), "cron.removed_orphan", &acct.ID, map[string]any{
				"project_id": insertedProject.ID,
				"slug":       slug,
				"err":        lerr.Error(),
			})
			continue
		}
		var appID string
		for _, a := range apps {
			if a.Slug == slug {
				appID = a.ID
				break
			}
		}
		if appID == "" {
			// The removed workload's app row was hard-deleted
			// somewhere (or never existed) — best-effort,
			// log the orphan.
			s.audit.Emit(r.Context(), "cron.removed_orphan", &acct.ID, map[string]any{
				"project_id": insertedProject.ID,
				"slug":       slug,
				"reason":     "no matching app row",
			})
			continue
		}
		// Soft-delete every cron attached to the removed
		// workload's app. The Cron row has no separate
		// workload_name column (one app = one workload in the
		// scan-tied model), so every cron on this app is
		// orphaned by the removal. DeleteCron is the
		// soft-delete primitive (status moves to
		// app-deleted via the parent row already being
		// soft-deleted by reconcile).
		cs, lerr := s.store.ListCronsForApp(r.Context(), appID)
		if lerr != nil {
			s.audit.Emit(r.Context(), "cron.removed_orphan", &acct.ID, map[string]any{
				"project_id": insertedProject.ID,
				"slug":       slug,
				"err":        lerr.Error(),
			})
			continue
		}
		for _, c := range cs {
			if err := s.store.DeleteCron(r.Context(), c.ID, appID); err != nil {
				s.audit.Emit(r.Context(), "cron.removed_orphan", &acct.ID, map[string]any{
					"project_id": insertedProject.ID,
					"slug":       slug,
					"cron_id":    c.ID,
					"err":        err.Error(),
				})
				continue
			}
			s.audit.Emit(r.Context(), "cron.removed", &acct.ID, map[string]any{
				"project_id": insertedProject.ID,
				"app_id":     appID,
				"cron_id":    c.ID,
				"slug":       slug,
			})
		}
	}

	// Build the slug→ID map for cron stamping. The cron list is
	// encoded by name in resp.CronNames; both Added and Changed
	// contribute to the map because a cron can attach to either a
	// newly-created or an updated app. Removed-slug crons were
	// soft-deleted above; the loop below only stamps NEW crons.
	slugToID := make(map[string]string, len(added)+len(changed))
	for _, a := range added {
		slugToID[a.Slug] = a.ID
	}
	for _, a := range changed {
		slugToID[a.Slug] = a.ID
	}
	for i, name := range resp.CronNames {
		appID := slugToID[name]
		if appID == "" {
			// H8 fix: removed workload — the cron for it was
			// soft-deleted in the loop above. This branch is
			// now a defensive no-op rather than a 500.
			continue
		}
		// The schedule + path + enabled flags ride on the planCron
		// entry captured earlier. resp.Crons has the same length
		// + order as resp.CronNames (scan_service.go populates
		// them in the same loop).
		if i >= len(resp.Crons) {
			continue
		}
		c := resp.Crons[i]
		if _, err := s.store.CreateCron(r.Context(), appID, c.Schedule, c.Path, c.Enabled); err != nil {
			api.WriteProblem(w, api.ErrInternal(
				fmt.Sprintf("stamp cron app_id: %v", err)))
			return
		}
	}

	// Notify every touched app. PostgreSQL delivers the
	// notification only on Tx commit; reconcile's CreateAppIfUnderQuota
	// + UpdateApp have already committed per-row (each call wraps
	// its own Tx), so the post-reconcile emit is safe — schedd
	// sees the rows.
	//
	// kind=created fires for Added; kind=updated fires for Changed.
	// The latter keeps schedd in sync if a workload's RootDir /
	// WorkloadName / StartCommand drifted across two applies.
	//
	// ctx is captured explicitly so the closure doesn't extend
	// the lifetime of r (contextcheck linter).
	notifyCtx := r.Context()
	notifyApp := func(a state.App, kind string) {
		_ = s.notif.Notify(notifyCtx, db.NotifyAppChanged,
			fmt.Sprintf(`{"kind":%q,"app_id":"%s","project_id":"%s"}`,
				kind, a.ID, insertedProject.ID))
	}
	for _, a := range added {
		notifyApp(a, "created")
	}
	for _, a := range changed {
		notifyApp(a, "updated")
	}

	// Audit + response. Response body mirrors the scan output but
	// carries the inserted/updated IDs so the CLI's --yes flow
	// can render "applied: <slug> → <app_id>".
	type applyResp struct {
		scanPlanResponse
		ProjectID string         `json:"project_id"`
		AppIDs    []appSummary   `json:"apps"`
		Builds    []appliedBuild `json:"builds,omitempty"`
	}
	appIDs := make([]appSummary, 0, len(added)+len(changed))
	for _, a := range added {
		appIDs = append(appIDs, appSummary{Slug: a.Slug, ID: a.ID})
	}
	for _, a := range changed {
		appIDs = append(appIDs, appSummary{Slug: a.Slug, ID: a.ID})
	}
	out := applyResp{
		scanPlanResponse: *resp,
		ProjectID:        insertedProject.ID,
		AppIDs:           appIDs,
		Builds:           builds,
	}

	s.audit.Emit(r.Context(), "project.created", &acct.ID, map[string]any{
		"project_id":  insertedProject.ID,
		"slug":        insertedProject.Slug,
		"scan_source": string(insertedProject.ScanSource),
		"app_count":   len(added) + len(changed),
	})

	// ADR-124 follow-up #3 (PR-B commit 5): write the operator's
	// persisted exclusions to deployment_scope_exclusions on a
	// successful apply when --persist-exclude was set. The
	// partition Skipped list carries every slug the operator
	// excluded (post-fallback, so persisted carry-forward slugs
	// are included if they were excluded this deploy). Idempotent
	// on duplicate (23505 → ErrConflict is treated as a no-op
	// because the row already exists from a prior deploy). Audit
	// row emitted per slug for SOC 2 CC7.2 paper trail.
	if resp.PersistExclude && len(resp.Skipped) > 0 {
		for _, skipped := range resp.Skipped {
			// Find the freshly-inserted app_id for this slug (it
			// exists because reconcile just inserted every
			// workload as a fresh app row on the apply path).
			var appID string
			for _, a := range added {
				if a.Slug == skipped.Slug {
					appID = a.ID
					break
				}
			}
			// The slug is in Skipped but didn't match an added
			// app — this can happen when the operator excluded an
			// existing app via --exclude (the audit fix at
			// TestReconcile_ExcludePreventsRemove covers the
			// apply-side contract). Lookup the existing app id.
			if appID == "" {
				if apps, lerr := s.store.AppsForProject(r.Context(), acct.ID, insertedProject.ID); lerr == nil {
					for _, a := range apps {
						if a.Slug == skipped.Slug {
							appID = a.ID
							break
						}
					}
				}
			}
			row, perr := s.store.CreateDeploymentScopeExclusion(r.Context(), state.DeploymentScopeExclusion{
				AccountID: acct.ID,
				ProjectID: insertedProject.ID,
				AppID:     appID, // empty is allowed: schema has no FK to apps(id)
				Slug:      skipped.Slug,
				Reason:    "persisted_via_flag",
				CreatedBy: "cli", // future: thread actor from the auth context
			})
			if perr != nil && perr != state.ErrConflict {
				// Don't fail the apply on a persist-write miss;
				// log the error via the audit log and continue.
				// The apply already succeeded — losing one
				// persist write is acceptable per the ADR-127 §1
				// audit-log-is-durable-record posture.
				s.audit.Emit(r.Context(), "project.scope.excluded", &acct.ID, map[string]any{
					"project_id":    insertedProject.ID,
					"workload_name": skipped.Slug,
					"reason":        "persist_write_failed",
					"err":           perr.Error(),
				})
				continue
			}
			// Audit row: only emit when we actually wrote the
			// row (ErrConflict means it already existed from a
			// prior deploy).
			if perr == nil && row.ID != "" {
				s.audit.Emit(r.Context(), "project.scope.excluded", &acct.ID, map[string]any{
					"project_id":    insertedProject.ID,
					"app_id":        row.AppID,
					"workload_name": row.Slug,
					"reason":        row.Reason,
				})
			}
		}
	}

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
//     403 plan_cron_quota        (Limit > 0 but exceeded)
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
