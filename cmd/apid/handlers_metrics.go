package main

// Per-app metrics handler (issue #273 / ADR-042).
//
// GET /v1/apps/{slug}/metrics?range=...
//
// Read-only, scoped to api.ScopesReadSurface (admin or apps:read).
// No MFA required — the primary caller is an API key. IDOR-safe via
// the existing loadApp (cross-account slug → 404, not 200 with
// another tenant's data).
//
// Range vocabulary is closed and bounded by Prometheus retention
// (`prom_retention_days: 15` in deploy/ansible/roles/prometheus/
// defaults/main.yml): see pkg/appmetrics.Ranges / IsValidRange.
//
// Prometheus unreachable (s.promqlClient == nil, or a query failed)
// → HTTP 200 with zeroed fields and Source="degraded: <reason>",
// matching the public /status/slo.json contract so the dashboard
// has one empty-state path. The PromQL builders + NaN/Inf guards
// live in pkg/appmetrics (extracted for issue #396 / ADR-045 PR 2
// so the meterd evaluator in PR 4 can share the same fetch path).

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/appmetrics"
	"github.com/onebox-faas/faas/pkg/state"
)

// getAppMetrics serves GET /v1/apps/{slug}/metrics?range=.
// Mirrors getApp's auth chain (without requireMFA — read-only,
// primary caller is an API key with ScopesReadSurface).
func (s *server) getAppMetrics(w http.ResponseWriter, r *http.Request, acct state.Account) {
	app, ok := s.loadApp(w, r, acct, r.PathValue("slug")) //nolint:contextcheck // loadApp takes r and uses r.Context() for its own DB calls; the helper is shared across every per-app handler.
	if !ok {
		// loadApp already wrote the 404.
		return
	}

	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = appmetrics.DefaultRange
	}
	if !appmetrics.IsValidRange(rng) {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
			"invalid range",
			fmt.Sprintf("range must be one of: %s", strings.Join(appmetrics.Ranges(), ", "))))
		return
	}

	resp, src := appmetrics.Fetch(r.Context(), s.promqlClient, s.log, app.ID, rng)
	resp.AppID = app.ID
	resp.Range = rng
	resp.Source = src
	resp.AsOf = time.Now().UTC().Format(time.RFC3339Nano)
	writeJSON(w, http.StatusOK, resp)
}
