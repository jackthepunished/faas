package main

// Per-app usage summary handler (per-app observability PR series,
// commit 4).
//
// GET /v1/apps/{slug}/usage?since=&until=
//
// Customer-facing billing summary for one app over a caller-
// supplied window (default: trailing 30d, clamped at 90d upper
// bound). Plan-gated Hobby+ — Free gets 402
// plan_app_usage_summary_not_allowed. The gate runs BEFORE loadApp
// so a Free customer probing a Hobby+ slug never gets a 404
// (slug-leak guard).
//
// Auth chain matches getAppMetrics (read-only, no MFA, primary
// caller is an API key with ScopesReadSurface). IDOR-safe via
// loadApp (cross-account slug → 404, byte-identical to a real
// 404).
//
// Window vocabulary: caller passes ?since= and ?until= in RFC3339.
// Both default to UTC midnight snaps; the handler clamps `since`
// to `until - 90d` upper bound. Empty input → defaults to
// trailing 30d ending at period_end = now().UTC() snapped down to
// UTC midnight (matches the dashboard's "this month" chip).
//
// Overage computation: max(0, gb_hours - plan_included). The
// helper pkg/meter.BuildAppWindowSummary rolls up the raw window;
// the handler is responsible for the plan-aware overage math so
// the helper stays plan-agnostic.

import (
	"net/http"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/meter"
	"github.com/onebox-faas/faas/pkg/state"
)

// usageMaxWindowDays is the upper bound on the trailing window
// the handler accepts. Clamps a customer-supplied `since` so the
// indexed scan on usage_minutes stays bounded (ADR-048 retention
// is 30d; the 90d ceiling is a forward-compatibility ceiling for
// when usage_daily lands). The ceiling is also the load-bearing
// guard against a customer accidentally scanning the full table
// with `since=1970-01-01`.
const usageMaxWindowDays = 90

// usageDefaultWindowDays is the trailing-window default for
// callers that omit both `since` and `until`. Picked to match
// ADR-048's usage_minutes retention so the dashboard's default
// "this month" view returns a complete rollup (no missing days
// at the trailing edge).
const usageDefaultWindowDays = 30

// getAppUsage serves GET /v1/apps/{slug}/usage. Returns
// api.AppUsageSummaryResponse. 200 on success, 402 on Free, 404
// on cross-account slug (via loadApp).
func (s *server) getAppUsage(w http.ResponseWriter, r *http.Request, acct state.Account) {
	if !acct.Plan.AppUsageSummaryAllowed() {
		api.WriteProblem(w, api.ErrPlanAppUsageSummaryNotAllowed(acct.Plan))
		return
	}
	app, ok := s.loadApp(w, r, acct, r.PathValue("slug")) //nolint:contextcheck // loadApp uses r.Context() for its own DB calls; helper is shared across every per-app handler.
	if !ok {
		return
	}

	since, until, err := parseUsageWindow(r, time.Now().UTC())
	if err != nil {
		api.WriteProblem(w, err)
		return
	}

	summary, source, sumErr := meter.BuildAppWindowSummary(r.Context(), s.store, acct.ID, app.ID, since, until)
	if sumErr != nil {
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError, api.CodeInternal,
			"usage summary fetch failed",
			"could not load usage summary; see server logs"))
		return
	}

	planIncluded := float64(acct.Plan.PlanIncludedGBHours())
	overage := summary.GBHours - planIncluded
	if overage < 0 {
		overage = 0
	}

	writeJSON(w, http.StatusOK, api.AppUsageSummaryResponse{
		Slug:                app.Slug,
		PeriodStart:         since.UTC(),
		PeriodEnd:           until.UTC(),
		MBSeconds:           summary.MBSeconds,
		GBHours:             summary.GBHours,
		Requests:            summary.Requests,
		TxBytes:             summary.TxBytes,
		BuilderSeconds:      summary.BuilderSeconds,
		ColdBootCount:       summary.ColdBootCount,
		PlanIncludedGBHours: planIncluded,
		OverageGBHours:      overage,
		Source:              source,
		AsOf:                time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// parseUsageWindow resolves the (since, until) half-open window
// from the URL query string. Both default to a trailing 30d
// window ending at UTC midnight; both clamp to RFC3339. The
// upper bound (since cannot be earlier than until - 90d) is the
// load-bearing guard against an unbounded indexed scan.
//
// Returns a *api.Problem so the handler can writeProblem directly
// on the error path without re-constructing the error envelope.
func parseUsageWindow(r *http.Request, now time.Time) (time.Time, time.Time, *api.Problem) {
	q := r.URL.Query()
	until := now.UTC()
	if raw := q.Get("until"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return time.Time{}, time.Time{}, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
				"invalid until",
				"until must be RFC3339")
		}
		until = t.UTC()
	} else {
		// Snap to UTC midnight so the period_end is a stable
		// day boundary. The dashboard's "this month" chip
		// re-uses the same snap.
		until = time.Date(until.Year(), until.Month(), until.Day(), 0, 0, 0, 0, time.UTC)
	}

	since := until.AddDate(0, 0, -usageDefaultWindowDays)
	if raw := q.Get("since"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return time.Time{}, time.Time{}, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
				"invalid since",
				"since must be RFC3339")
		}
		since = t.UTC()
	}

	// Clamp `since` to the upper bound so a customer cannot
	// unbounded-scan usage_minutes. Without this guard,
	// since=1970-01-01 would walk the full table.
	minSince := until.AddDate(0, 0, -usageMaxWindowDays)
	if since.Before(minSince) {
		since = minSince
	}
	if since.After(until) {
		return time.Time{}, time.Time{}, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
			"invalid window",
			"since must be earlier than until")
	}
	return since, until, nil
}
