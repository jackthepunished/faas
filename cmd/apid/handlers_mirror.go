// Traffic mirroring HTTP handlers (issue #72 / ADR-125 traffic
// mirroring PR-A2). Six routes live under /v1/apps/{slug}/mirrors:
//
//	POST   /v1/apps/{slug}/mirrors                createMirrorRule
//	GET    /v1/apps/{slug}/mirrors                listMirrorRules
//	GET    /v1/apps/{slug}/mirrors/{id}           getMirrorRule
//	PATCH  /v1/apps/{slug}/mirrors/{id}           updateMirrorRule
//	DELETE /v1/apps/{slug}/mirrors/{id}           deleteMirrorRule
//	GET    /v1/apps/{slug}/mirrors/{id}/summary   getMirrorRuleSummary
//
// PR-A2 ships the CRUD surface only — the runtime mirror goroutine
// (detached ctx, redaction, schedd stamping) lands in PR-A3. By the
// end of PR-A2, a customer can POST a rule and see it via GET, but no
// traffic is actually mirrored yet; the GET /summary endpoint reads
// rows from the comparison ledger that PR-A1 left empty.
//
// Gate order (mirrors updateDeploymentTraffic in handlers_ext.go and
// createEdgeRule in handlers_edge_rules.go exactly):
//
//  1. Resolve app via loadApp(slug) — IDOR guard: cross-account slug
//     returns silent 404.
//  2. Decode body; reject unknown JSON fields (decodeJSON's
//     DisallowUnknownFields).
//  3. Range check FIRST (no plan context) — 422 invalid_mirror_percent.
//  4. Plan tier gate — 403 plan_mirror_not_allowed (Hobby/Free locked).
//  5. Store call (transactional; FOR UPDATE on apps row enforces the
//     per-app quota).
//  6. Audit emit (best-effort) — kind=mirror_rule.{created,updated,deleted}.
//  7. pg_notify — kind="mirror" on NotifyDeploymentChanged; gateway
//     refresh subscriber (PR-A3) picks this up.
//
// Range-before-plan is intentional: a malformed value is loud
// regardless of plan, and the plan gate only fires on a legal value
// (so the operator sees the 403 "plan locked" not a 422 "value
// illegal"). Pattern matches handlers_ext.go::updateDeploymentTraffic.
package main

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/state"
)

// mirrorRuleResponse maps a state.MirrorRule to the customer-facing
// api.MirrorRuleResponse shape. Centralised so every handler
// (POST/GET/PATCH/DELETE/list) emits byte-identical JSON for the
// same row — including the always-stripped headers manifest, which
// the customer renders in their UI to know what the gateway
// guarantees regardless of their redact_headers setting.
func mirrorRuleResponse(r state.MirrorRule) api.MirrorRuleResponse {
	return api.MirrorRuleResponse{
		ID:                    r.ID,
		AccountID:             r.AccountID,
		AppID:                 r.AppID,
		SourceDeploymentID:    r.SourceDeploymentID,
		MirrorDeploymentID:    r.MirrorDeploymentID,
		Percent:               r.Percent,
		Enabled:               r.Enabled,
		IncludeBody:           r.IncludeBody,
		RedactHeaders:         r.RedactHeaders,
		AlwaysStrippedHeaders: api.MirrorAlwaysStrippedHeaders,
		CreatedAt:             r.CreatedAt,
		UpdatedAt:             r.UpdatedAt,
	}
}

// createMirrorRule is the handler for POST /v1/apps/{slug}/mirrors.
// Issue #72 / ADR-125 PR-A2. Gate order is documented at the top
// of this file; this is the canonical mirror create path.
func (s *server) createMirrorRule(w http.ResponseWriter, r *http.Request, acct state.Account) {
	slug := r.PathValue("slug")
	app, ok := s.loadApp(w, r, acct, slug)
	if !ok {
		return
	}
	var req api.CreateMirrorRuleRequest
	if err := decodeJSON(r, &req); err != nil {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation, "Bad request", err.Error()))
		return
	}
	// Range check FIRST (no plan context). 422 invalid_mirror_percent
	// with the cap + observed value so the CLI renders actionable
	// retry guidance. Range-before-plan is intentional (see file
	// header).
	if req.Percent < 0 || req.Percent > 100 {
		api.WriteProblem(w, api.ErrInvalidMirrorPercent(req.Percent))
		return
	}
	// Plan tier gate. Hobby/Free always see 403 plan_mirror_not_allowed
	// (the mirror bill is N×ram_mb×seconds per request, distinct from
	// traffic split's canary floor — see Limits.MirrorRuleAllowed's
	// comment in pkg/api/limits.go). 403 (not 422) because the
	// request shape is legal; only the plan forbids it.
	if !acct.Plan.MirrorRuleAllowed() {
		api.WriteProblem(w, api.ErrPlanMirrorNotAllowed(acct.Plan))
		return
	}
	// RedactHeaders nil-safety: the SQL DEFAULT is '{}', so an
	// empty slice round-trips identically. nil and []string{} both
	// pass through CreateMirrorRuleIfUnderQuota's array_length check.
	redact := req.RedactHeaders
	if redact == nil {
		redact = []string{}
	}
	limits := api.MustLimitsFor(acct.Plan)
	created, err := s.store.CreateMirrorRuleIfUnderQuota(r.Context(),
		state.CreateMirrorRuleParams{
			AccountID:          acct.ID,
			AppID:              app.ID,
			SourceDeploymentID: req.SourceDeploymentID,
			MirrorDeploymentID: req.MirrorDeploymentID,
			Percent:            req.Percent,
			Enabled:            true,
			IncludeBody:        req.IncludeBody,
			RedactHeaders:      redact,
		}, limits)
	if err != nil {
		switch {
		case errors.Is(err, state.ErrInvalidMirrorPercent):
			api.WriteProblem(w, api.ErrInvalidMirrorPercent(req.Percent))
		case errors.Is(err, state.ErrMirrorSourceTargetSame):
			api.WriteProblem(w, api.ErrMirrorSourceTargetSame())
		case errors.Is(err, state.ErrMirrorDeploymentNotLive):
			api.WriteProblem(w, api.ErrMirrorDeploymentNotLive())
		case errors.Is(err, state.ErrMirrorCrossAppMismatch):
			api.WriteProblem(w, api.ErrMirrorCrossAppMismatch())
		default:
			var qe *state.QuotaError
			if errors.As(err, &qe) && qe.Kind == state.QuotaErrorKindMirror {
				if qe.NotAllowed {
					// Plan tier rejected inside the transaction
					// (defence-in-depth; the apid gate above
					// catches Hobby/Free first, but the store
					// also runs the gate so direct callers
					// can't bypass it).
					api.WriteProblem(w, api.ErrPlanMirrorNotAllowed(acct.Plan))
					return
				}
				api.WriteProblem(w, api.ErrMirrorRuleQuotaExceeded(limits, qe.Observed))
				return
			}
			api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError, api.CodeInternal, "could not create mirror rule", err.Error()))
		}
		return
	}
	if s.audit != nil {
		s.audit.Emit(r.Context(), "mirror_rule.created", &acct.ID, map[string]any{
			"app":          app.ID,
			"rule":         created.ID,
			"source":       created.SourceDeploymentID,
			"mirror":       created.MirrorDeploymentID,
			"percent":      created.Percent,
			"include_body": created.IncludeBody,
		})
	}
	// pg_notify so PR-A3's gateway refresh subscriber reloads the
	// rule set within ~1s. Same channel as traffic-split
	// (NotifyDeploymentChanged); the kind="mirror" discriminant
	// distinguishes. Pre-PR-A3 the gateway ignores this signal;
	// the audit + row write still happen so the customer sees the
	// rule via GET. Best-effort: a notify outage is logged and
	// continued (matches the traffic-split pattern at
	// handlers_ext.go:1549-1554).
	if s.notif != nil {
		if err := s.notif.Notify(r.Context(), db.NotifyDeploymentChanged,
			fmt.Sprintf(`{"kind":"mirror","app_id":"%s","source_deployment_id":"%s","mirror_deployment_id":"%s","rule_id":"%s"}`,
				app.ID, created.SourceDeploymentID, created.MirrorDeploymentID, created.ID)); err != nil {
			s.log.Warn("apid: notify deployment_changed (mirror create) failed", "err", err)
		}
	}
	writeJSON(w, http.StatusCreated, mirrorRuleResponse(created))
}

// listMirrorRules is the handler for GET /v1/apps/{slug}/mirrors.
// Issue #72 / ADR-125 PR-A2. ListMirrorRules returns at most
// Limits.MirrorTargetsPerApp rows (1-3) so no pagination cursor
// is needed in A2. The {id} routes below this one apply the
// same IDOR posture (silent 404 on cross-account).
func (s *server) listMirrorRules(w http.ResponseWriter, r *http.Request, acct state.Account) {
	slug := r.PathValue("slug")
	app, ok := s.loadApp(w, r, acct, slug)
	if !ok {
		return
	}
	rules, err := s.store.ListMirrorRules(r.Context(), app.ID)
	if err != nil {
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError, api.CodeInternal, "could not list mirror rules", err.Error()))
		return
	}
	out := make([]api.MirrorRuleResponse, 0, len(rules))
	for i := range rules {
		out = append(out, mirrorRuleResponse(rules[i]))
	}
	writeJSON(w, http.StatusOK, api.MirrorRuleListResponse{Rules: out, Count: len(out)})
}

// loadMirrorRuleIfOwned resolves a mirror rule by id and verifies
// it belongs to the calling account AND to the slug's app. Returns
// (rule, true) on success; writes a silent 404 and returns false
// on missing / cross-account / cross-app — the IDOR-safe posture
// that all mirror {id}-routes use. The cross-app check protects
// against a future regression that swaps app.ID for a different
// loadApp result (e.g. an org-scoped URL path).
func (s *server) loadMirrorRuleIfOwned(w http.ResponseWriter, r *http.Request, acct state.Account, app state.App, id string) (state.MirrorRule, bool) {
	rule, err := s.store.GetMirrorRuleByID(r.Context(), id)
	if err != nil {
		s.notFound(w, "no such mirror rule")
		return state.MirrorRule{}, false
	}
	if rule.AccountID != acct.ID || rule.AppID != app.ID {
		s.notFound(w, "no such mirror rule")
		return state.MirrorRule{}, false
	}
	return rule, true
}

// getMirrorRule is the handler for GET /v1/apps/{slug}/mirrors/{id}.
// Issue #72 / ADR-125 PR-A2. Single-row read with the IDOR guard
// from loadMirrorRuleIfOwned.
func (s *server) getMirrorRule(w http.ResponseWriter, r *http.Request, acct state.Account) {
	slug := r.PathValue("slug")
	app, ok := s.loadApp(w, r, acct, slug)
	if !ok {
		return
	}
	rule, ok := s.loadMirrorRuleIfOwned(w, r, acct, app, r.PathValue("id"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, mirrorRuleResponse(rule))
}

// updateMirrorRule is the handler for PATCH /v1/apps/{slug}/mirrors/{id}.
// Issue #72 / ADR-125 PR-A2. MirrorRulePatch's pointer fields
// distinguish "field absent" from "field set to zero" — Percent=0
// is a legal value (disable without removing). Range check fires
// only when Percent is set to a non-nil out-of-range value. Plan
// gate is skipped on update (a Pro customer's existing rule
// survives an upgrade to Hobby — they just can't *create* a new
// one; the rule is disabled by the reaper at the next read
// window so the mirror VM doesn't keep waking). This matches the
// traffic-split precedent where the plan gate fires on the create
// path but not the update path.
func (s *server) updateMirrorRule(w http.ResponseWriter, r *http.Request, acct state.Account) {
	slug := r.PathValue("slug")
	app, ok := s.loadApp(w, r, acct, slug)
	if !ok {
		return
	}
	rule, ok := s.loadMirrorRuleIfOwned(w, r, acct, app, r.PathValue("id"))
	if !ok {
		return
	}
	var req api.UpdateMirrorRuleRequest
	if err := decodeJSON(r, &req); err != nil {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation, "Bad request", err.Error()))
		return
	}
	// Range check ONLY when Percent is supplied. A missing Percent
	// keeps the existing value (the patch semantics).
	if req.Percent != nil && (*req.Percent < 0 || *req.Percent > 100) {
		api.WriteProblem(w, api.ErrInvalidMirrorPercent(*req.Percent))
		return
	}
	prev := rule
	updated, err := s.store.UpdateMirrorRule(r.Context(), rule.ID, state.MirrorRulePatch{
		Percent:       req.Percent,
		Enabled:       req.Enabled,
		IncludeBody:   req.IncludeBody,
		RedactHeaders: req.RedactHeaders,
	})
	if err != nil {
		switch {
		case errors.Is(err, state.ErrInvalidMirrorPercent):
			// Defence-in-depth: handler already range-checked,
			// but the store re-validates and surfaces the same
			// sentinel.
			if req.Percent != nil {
				api.WriteProblem(w, api.ErrInvalidMirrorPercent(*req.Percent))
			} else {
				api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError, api.CodeInternal, "update failed", err.Error()))
			}
		case errors.Is(err, state.ErrNotFound):
			s.notFound(w, "no such mirror rule")
		default:
			api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError, api.CodeInternal, "update failed", err.Error()))
		}
		return
	}
	if s.audit != nil {
		s.audit.Emit(r.Context(), "mirror_rule.updated", &acct.ID, map[string]any{
			"app":          app.ID,
			"rule":         updated.ID,
			"source":       updated.SourceDeploymentID,
			"mirror":       updated.MirrorDeploymentID,
			"percent":      updated.Percent,
			"enabled":      updated.Enabled,
			"include_body": updated.IncludeBody,
			"prev": map[string]any{
				"percent":      prev.Percent,
				"enabled":      prev.Enabled,
				"include_body": prev.IncludeBody,
			},
		})
	}
	if s.notif != nil {
		if err := s.notif.Notify(r.Context(), db.NotifyDeploymentChanged,
			fmt.Sprintf(`{"kind":"mirror","app_id":"%s","source_deployment_id":"%s","mirror_deployment_id":"%s","rule_id":"%s"}`,
				app.ID, updated.SourceDeploymentID, updated.MirrorDeploymentID, updated.ID)); err != nil {
			s.log.Warn("apid: notify deployment_changed (mirror update) failed", "err", err)
		}
	}
	writeJSON(w, http.StatusOK, mirrorRuleResponse(updated))
}

// deleteMirrorRule is the handler for DELETE /v1/apps/{slug}/mirrors/{id}.
// Issue #72 / ADR-125 PR-A2. Returns 204 No Content; the
// mirror_invocation_results rows cascade via FK ON DELETE CASCADE
// (migration 00384_mirror_rules.sql). Idempotent at the wire level:
// a second DELETE returns 404 (state.ErrNotFound surfaces as a
// silent 404 to match the IDOR posture — cross-account probing
// cannot distinguish "exists" from "deleted").
func (s *server) deleteMirrorRule(w http.ResponseWriter, r *http.Request, acct state.Account) {
	slug := r.PathValue("slug")
	app, ok := s.loadApp(w, r, acct, slug)
	if !ok {
		return
	}
	rule, ok := s.loadMirrorRuleIfOwned(w, r, acct, app, r.PathValue("id"))
	if !ok {
		return
	}
	if err := s.store.DeleteMirrorRule(r.Context(), rule.ID); err != nil {
		switch {
		case errors.Is(err, state.ErrNotFound):
			s.notFound(w, "no such mirror rule")
		default:
			api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError, api.CodeInternal, "delete failed", err.Error()))
		}
		return
	}
	if s.audit != nil {
		s.audit.Emit(r.Context(), "mirror_rule.deleted", &acct.ID, map[string]any{
			"app":    app.ID,
			"rule":   rule.ID,
			"source": rule.SourceDeploymentID,
			"mirror": rule.MirrorDeploymentID,
		})
	}
	if s.notif != nil {
		if err := s.notif.Notify(r.Context(), db.NotifyDeploymentChanged,
			fmt.Sprintf(`{"kind":"mirror","app_id":"%s","source_deployment_id":"%s","mirror_deployment_id":"%s","rule_id":"%s"}`,
				app.ID, rule.SourceDeploymentID, rule.MirrorDeploymentID, rule.ID)); err != nil {
			s.log.Warn("apid: notify deployment_changed (mirror delete) failed", "err", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// getMirrorRuleSummary is the handler for
// GET /v1/apps/{slug}/mirrors/{id}/summary?window={1h|24h|7d}.
// Issue #72 / ADR-125 PR-A2. The store's MirrorSummary aggregates
// rows via SQL COUNT/SUM/p99_cont; the handler is a thin shell
// over the parse + translate. Read surface — no audit/notify
// emit. The {id}-route IDOR guard applies (silent 404 on
// cross-account).
func (s *server) getMirrorRuleSummary(w http.ResponseWriter, r *http.Request, acct state.Account) {
	slug := r.PathValue("slug")
	app, ok := s.loadApp(w, r, acct, slug)
	if !ok {
		return
	}
	rule, ok := s.loadMirrorRuleIfOwned(w, r, acct, app, r.PathValue("id"))
	if !ok {
		return
	}
	windowStr := r.URL.Query().Get("window")
	window, err := api.ParseMirrorWindow(windowStr)
	if err != nil {
		api.WriteProblem(w, api.ErrInvalidMirrorWindow(windowStr))
		return
	}
	// MirrorSummary's `since` argument is "rows whose
	// completed_at >= now - window_seconds". Computed via
	// time.Now() inline; no test seam in A2 (PR-A3's redaction
	// layer may want one for golden tests).
	since := timeNow().Add(-time.Duration(window) * time.Second)
	summary, err := s.store.MirrorSummary(r.Context(), rule.ID, since)
	if err != nil {
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError, api.CodeInternal, "could not compute mirror summary", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, api.MirrorSummaryResponse{
		TotalInvocations:  int64(summary.TotalInvocations),
		StatusDiffCount:   int64(summary.StatusDiffCount),
		SchemaDiffCount:   int64(summary.SchemaDiffCount),
		BodyDiffCount:     int64(summary.BodyDiffCount),
		MeanLatencyDiffMs: int64(summary.MeanLatencyDiffMs),
		P99LatencyDiffMs:  int64(summary.P99LatencyDiffMs),
		CrashCount:        int64(summary.CrashCount),
		WindowSeconds:     int(window),
	})
}

// The summary handler deliberately stays narrow: SQL aggregates
// in MirrorSummary already return the typed MirrorSummary struct,
// and the api.MirrorSummaryResponse shape is a 1:1 map (the only
// field-level translation is int → int64 for forward-compat with
// the pg-side count() return type).
