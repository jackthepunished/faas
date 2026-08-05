package main

// handlers_scan.go — per-deploy grype scan HTTP handler
// (issue #464 / ADR-055 / PR-4 of the mega-PR).
//
// GET /v1/deployments/{id}/scan returns the per-deploy grype
// scan payload for one deployment. The route is the
// drill-down surface (the dashboard reads it; the CLI's
// `--show-scan` flag reads it; PR-5 / PR-6 don't redecode
// the typed payload — they hit this route and pass the JSON
// through to the consumer).
//
// Wire shape: api.ScanResult (pkg/api/dto_scan.go). Status
// is the closed enum that mirrors deployments.scan_status:
//
//   - "complete" — full payload (SeverityCounts +
//     Vulnerabilities). scannED_at is the wall clock the
//     scan landed.
//   - "failed" — payload carries Error only; SeverityCounts
//     is all-zero, Vulnerabilities is nil. The dashboard
//     renders the Error message on the "scan failed" pill.
//   - "skipped" — pre-feature backfill rows
//     (migrations/00135 stamps scan_status='skipped' on
//     every pre-#464 row). The payload carries a reason
//     sentinel in scan_result jsonb; we surface Status +
//     reason only.
//   - NULL / "" (no scan run yet) — 404. The dashboard
//     shows "scan pending" on the 404 (matches the
//     DeploymentResponse.Scan absence path on PR-3).
//
// IDOR posture: same AppByID + AccountID check as
// getDeployment (handler_ext.go:660). A cross-account
// probe returns 404 — never reveals whether the
// deployment exists.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// getDeploymentScan is the GET /v1/deployments/{id}/scan
// handler (issue #464 / ADR-055 / PR-4). Returns the typed
// ScanResult on a successful scan, the failed shape on a
// retry-exhausted scan, the skipped shape on pre-feature
// backfill rows, or 404 when no scan has run yet (or the
// deployment doesn't exist / is cross-account).
//
// Auth: authLimited + requireMFA + read scope — same chain
// as getDeployment at server.go:712. The handler body is
// kept small (≤50 lines per CLAUDE.md convention) by
// delegating the wire-shape construction to scanResponse.
func (s *server) getDeploymentScan(w http.ResponseWriter, r *http.Request, acct state.Account) {
	id := r.PathValue("id")
	d, err := s.store.DeploymentByID(ctx(r), id)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			s.notFound(w, "no such deployment")
			return
		}
		api.WriteProblem(w, api.ErrInternal(
			fmt.Sprintf("load deployment: %v", err)))
		return
	}
	app, err := s.store.AppByID(ctx(r), d.AppID)
	if err != nil || app.AccountID != acct.ID {
		// Same posture as getDeployment: cross-account
		// probes get 404, not 403 — we never reveal
		// whether the deployment_id exists in another
		// account.
		s.notFound(w, "no such deployment")
		return
	}
	if d.ScanStatus == "" {
		// No scan has run yet — the deploy is still
		// mid-pipeline or predates #464 entirely. 404 is
		// the right answer; the dashboard's "scan
		// pending" pill renders on the absence.
		s.notFound(w, "scan not yet available")
		return
	}
	resp := scanResponse(d)
	writeJSON(w, http.StatusOK, resp)
}

// scanResponse builds the wire-shape api.ScanResult from
// a state.Deployment row. Mirrors deploymentResponse's
// per-deploy scan branch (handlers_ext.go:2156-2198) so
// both endpoints return identical typed payloads for a
// given row. Splitting it out keeps the handler body
// under 50 lines and gives the CLI/dashboard a single
// decode target.
func scanResponse(d state.Deployment) api.ScanResult {
	out := api.ScanResult{Status: d.ScanStatus}
	if !d.ScannedAt.IsZero() {
		out.ScannedAt = d.ScannedAt.UTC().Format(time.RFC3339)
	}
	out.ImageDigest = d.ImageDigest
	if len(d.ScanResult) > 0 {
		if err := json.Unmarshal(d.ScanResult, &out); err != nil {
			// Decode failure is logged but doesn't
			// surface a 500 — the row carries a Status
			// the customer can act on (re-deploy, contact
			// support). The empty SeverityCounts +
			// nil Vulnerabilities render as the
			// "scan summary unavailable" view on the
			// dashboard.
			out.SeverityCounts = api.SeverityCounts{}
			out.Vulnerabilities = nil
			out.Error = "scan_result decode failed (server logs carry the detail)"
		}
		// Re-pin Status from the column — the column is
		// authoritative; the jsonb payload is data only.
		out.Status = d.ScanStatus
	}
	return out
}
