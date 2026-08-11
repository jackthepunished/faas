// Dashboard fire-now cron path (issue #791 PR-E / ADR-090
// §"Sub-decision 7"). Mirrors dashboard_delete.go's CSRF-envelope
// pattern (review finding A3): the renderer mints a sealed token
// bound to (action="fire_cron", account_id) via IssueForAuthenticated
// and sets it both as the faas_csrf sidecar cookie and as the form's
// csrf_token hidden field. The POST handler verifies it against the
// same binding before enqueueing the fire-now row.
//
// Two reasons we DO NOT reuse the v1 fire-now API path
// (POST /v1/crons/{id}/run):
//
//  1. The dashboard's links are <form method="POST">, not XHR —
//     relying on the v1 endpoint would require a JS shim.
//  2. The CSRF posture differs: v1 Bearer-key endpoints don't need a
//     form-binding token; the dashboard does. Reusing the v1 handler
//     would force a bearer-key-on-form split that breaks both routes.
//
// Both paths must still result in a single durable row in
// cron_fire_now_requests (inserted via InsertFireNowRequest) and a
// pg_notify NotifyCronRunNow — the difference is only at the request
// envelope. The handler delegates to the existing fireCronNow logic
// by issuing the insert + notify pair directly, identical to the
// json-going v1 path.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/dashboard"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/middleware"
	"github.com/onebox-faas/faas/pkg/state"
)

// dashboardFireCronAction is the CSRF action binding for the cron
// fire-now forms on /dashboard/apps/{slug}. Shared across every cron
// on the page (an account-wider token rather than per-cron is fine:
// the token verifies identity + intent, not which row; the lookup
// inside the handler revalidates ownership against the URL path).
const dashboardFireCronAction = "fire_cron"

// Dashboard cron-run glyph vocabulary (issue #791 PR-E / ADR-090).
// Mirrors the closed vocab the CLI's "crons runs" command prints
// (commands_crons_runs.go::formatCronRunsDuration) so a customer
// reading both surfaces agrees on one symbol per outcome.
const (
	glyphOK      = "✓"
	glyphFail    = "✗"
	glyphRunning = "⟳"
	// cronRunsEmDash is the sentinel for "no row" (missing
	// duration on a never-completing run, or a never-fired cron).
	// Re-used from the dashboard package's dashboardEmDash to keep
	// the value aligned across cron + instance run panels.
	cronRunsEmDash = "—"
)

// renderDashboardFireCron handles POST
// /dashboard/apps/{slug}/crons/{id}/fire-now. The form posts here
// with a sealed csrf_token (minted by renderAppDetail on GET); we
// verify against the (action="fire_cron", account_id) sealed
// envelope, resolve the same two-step ownership probe as
// handlers_ext.go::fireCronNow (CronByID → AppByID → AccountID), then
// insert the durable row and emit pg_notify. On any failure branch
// the customer lands back on the app-detail page with an
// `?fired=…` flash flag the template reads and surfaces.
//
// Form fields:
//
//	csrf_token — required; rendered by renderAppDetail as a hidden
//	             <input name="csrf_token" value="{{…}}">.
//
// Returns 302 to /dashboard/apps/{slug}#cron-{id}?fired=1 on success
// (303 once OpenAPI lands and the form is upgraded with
// `Accept: application/json`). The StatusFound matches the existing
// dashboardDelete / dashboardRestore / dashboardRaiseOverageCap
// handlers — no drift.
func (s *server) renderDashboardFireCron(w http.ResponseWriter, r *http.Request, slug, cronID string) {
	acct, ok := AccountFrom(r.Context())
	if !ok {
		// sessionAuth would have redirected; defensive 401.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := middleware.VerifyAuthenticated(s.sessions, r, dashboardFireCronAction, acct.ID); err != nil {
		// Same shape as dashboardDelete's CSRF-mismatch handling —
		// the helper wraps ErrCSRFInvalid on every failure path, so
		// the message is intentionally generic ("please reload and
		// try again") rather than naming which check tripped.
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
			"Invalid CSRF token", "please reload the page and try again"))
		return
	}
	if err := s.fireCronFromDashboard(r.Context(), acct, slug, cronID); err != nil {
		// Surface via redirect + flash flag; the template renders
		// the ?fired=…=error banner. Avoids leaving the operator
		// on a handler-error page where the back button fights
		// the form re-POST.
		http.Redirect(w, r, "/dashboard/apps/"+slug+"?fired=error", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/dashboard/apps/"+slug+"#cron-"+cronID+"?fired=1", http.StatusFound)
}

// dashboardFireCron is the mux.HandleFunc entrypoint (POST
// /dashboard/apps/{slug}/crons/{id}/fire-now). Decodes the
// path params and delegates to renderDashboardFireCron for the
// real work; lives next to the handler it serves so the
// mux.HandleFunc registration in server.go can keep one
// s.dashboardFireCron reference without stutter.
func (s *server) dashboardFireCron(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	id := r.PathValue("id")
	s.renderDashboardFireCron(w, r, slug, id)
}

// fireCronFromDashboard is the reusable core for the dashboard POST:
// load the app, load the cron, run the IDOR-safe two-step, enqueue
// fire-now, emit pg_notify. Returns nil on success and a typed
// dashboard error (ErrDashboardCronNotFound /
// ErrDashboardCronPlanBlocked / wrapped insert error) on failure.
//
// Split from renderDashboardFireCron so future dashboard variants
// (e.g. a JSON Accept-coded fire-now endpoint for the mobile page)
// can reuse the same ownership + insert logic.
//
// Split from renderDashboardFireCron so future dashboard variants
// (e.g. a JSON Accept-coded fire-now endpoint for the mobile page)
// can reuse the same ownership + insert logic.
func (s *server) fireCronFromDashboard(ctx context.Context, acct state.Account, slug, cronID string) error {
	app, err := s.store.AppBySlug(ctx, slug)
	if err != nil || app.AccountID != acct.ID {
		// Byte-identical 404 to the "missing" branch in the v1
		// handler — never reveal whether the app exists on another
		// account. The redirect-with-flash will surface as a
		// generic "fired=error" string, not as the underlying
		// reason, so the operator-visible path doesn't double as
		// an existence oracle either.
		return ErrDashboardCronNotFound
	}
	c, err := s.store.CronByID(ctx, cronID)
	if err != nil || c.AppID != app.ID {
		// IDOR-safe: cron must belong to THIS app, not just to
		// some app owned by the same account. A cron id from
		// another app on the same account must not enqueue.
		return ErrDashboardCronNotFound
	}
	// Plan-tier gate mirrors fireCronNow at handlers_cron_run.go:80.
	// Free customers never create a row that schedd will then
	// stamp as failed; the same ErrPlanCronsNotAllowed surfaces on
	// the redirect-with-flash back to the page rather than a 402.
	limits, ok := api.LimitsFor(acct.Plan)
	if !ok || limits.CronLimitPerApp == 0 {
		return ErrDashboardCronPlanBlocked
	}
	requestID, err := s.store.InsertFireNowRequest(ctx, c.ID, acct.ID)
	if err != nil {
		return fmt.Errorf("insert fire-now: %w", err)
	}
	if err := s.notif.Notify(ctx, db.NotifyCronRunNow,
		`{"request_id":"`+requestID+`"}`); err != nil {
		// Notify failures do NOT fail the request — the row is the
		// source of truth and schedd's 60s safety tick will pick it
		// up. Logged for monitoring.
		s.log.Warn("dashboard: cron fire-now notify failed; safety tick will recover",
			"cron_id", c.ID, "request_id", requestID, "err", err)
	}
	s.log.Info("dashboard: cron fire-now enqueued",
		"cron_id", c.ID, "app_id", c.AppID,
		"account_id", acct.ID, "request_id", requestID,
		"surface", "dashboard")
	return nil
}

// dashboardCronErrors are the typed reasons the dashboard POST can
// fail. The handler maps them to redirect-with-flash URIs; the
// reasons don't surface to the operator verbatim because doing so
// would either be 200-too-many-status-codes or leak the existence
// oracle. They live here so a future JSON Accept-coded variant
// can downgrade the redirect into an api problem without a
// separate error catalog.

var (
	// ErrDashboardCronNotFound covers the IDOR-safe 404 branch
	// (missing app, missing cron, cross-app cron). Always maps to
	// the same dashboard-fired=error flash; the v1 handler is the
	// one that distinguishes these for its byte-identical-404
	// contract.
	ErrDashboardCronNotFound = errors.New("dashboard: cron not found")
	// ErrDashboardCronPlanBlocked covers a Free-plan customer
	// trying to fire a cron (the rule still exists because it
	// landed before a plan downgrade). The dashboard surfaces the
	// same redirect-with-flash; the v1 endpoint surfaces the
	// ErrPlanCronsNotAllowed 402. Split so we can lift the
	// message later.
	ErrDashboardCronPlanBlocked = errors.New("dashboard: cron not allowed on this plan")
)

// projectCronRunRow collapses the CronRun projection down to the
// four fields the dashboard renders. The handler does all the
// formatting (duration in "1.2s" / "980ms" / "timeout" / "—", time
// in HH:MM) so the template can stay a pure renderer — same pattern
// as RecentInstanceItem and InstanceChipDurationMS. A NULL outcome
// surfaces as api.CronRunRunning (the server NEVER returns an empty
// string), so this translator does no nil-vs-empty dance.
func projectCronRunRow(inv state.Invocation) dashboard.CronRunRow {
	started := cronRunsEmDash
	if !inv.CreatedAt.IsZero() {
		started = inv.CreatedAt.UTC().Format("15:04")
	}
	outcome := api.CronRunRunning
	glyph := glyphRunning
	switch {
	case inv.Outcome == nil && inv.CompletedAt == nil:
		// Non-terminal — already defaulted to running.
	default:
		switch {
		case inv.Outcome != nil && *inv.Outcome == state.OutcomeSuccess:
			outcome = api.CronRunSuccess
			glyph = glyphOK
		case inv.Outcome != nil && *inv.Outcome == state.OutcomeTimeout:
			outcome = api.CronRunTimeout
			glyph = glyphFail
		case inv.Outcome != nil && *inv.Outcome == state.OutcomeDeadLetter:
			outcome = api.CronRunDeadLetter
			glyph = glyphFail
		case inv.Outcome != nil && *inv.Outcome == state.OutcomeFailed:
			outcome = api.CronRunFailed
			glyph = glyphFail
		default:
			outcome = api.CronRunFailed
			glyph = glyphFail
		}
	}
	dur := cronRunsEmDash
	if inv.CompletedAt != nil && !inv.CreatedAt.IsZero() {
		ms := inv.CompletedAt.Sub(inv.CreatedAt).Milliseconds()
		dur = formatCronRunDuration(ms)
	}
	return dashboard.CronRunRow{
		Glyph:      glyph,
		StartedAt:  started,
		DurationMS: dur,
		Outcome:    string(outcome),
	}
}

// formatCronRunDuration picks the dashboard's density-preserving
// short form. We honour the same time bands the CLI uses
// (commands_crons_runs.go) so a customer reading both surfaces
// doesn't see one column say "980ms" and the other "1.0s" for the
// same underlying run.
func formatCronRunDuration(ms int64) string {
	switch {
	case ms <= 0:
		return cronRunsEmDash
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 10_000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	default:
		// Cron run timeout is 10 min (build VM), but exec-worker
		// runs cap at 60s/300s/…; emit seconds for steady-state
		// sanity and let the customer scroll to the row label
		// to see "timeout".
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	}
}

// parseAppCronFirePath removed: the mux path
// /dashboard/apps/{slug}/crons/{id}/fire-now is registered
// directly in server.go (mux.HandleFunc with explicit slugs +
// ids), so the path-suffix parser never had a real caller. A
// follow-up that adds a drill-down
// /dashboard/apps/{slug}/crons/{id} renderer can copy the
// shape from this comment if it re-adds one.