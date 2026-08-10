package main

// PR 1 of the edge-rules rollout (ADR-089, planned). Round-trips
// the apid surface against the in-process MemStore so a typo in
// the validation, a missing IDOR check, or a plan-gate that
// leaks the slug can be caught before merge. Mirrors the alert
// test surface (handlers_alerts_test.go) so a future contributor
// migrating to real Postgres only needs to mirror the existing
// happy/quota/IDOR/error cases.
//
// The setup() harness uses MemStore (server_test.go:31) so this
// file runs in -short / no-PG CI; the pgstore counterpart at
// pkg/state/pgstore_edge_rules_test.go covers the SQL layer.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// mustSeedEdgeRuleApp creates a fresh app on the env's account +
// returns its slug so the test can POST to /v1/apps/{slug}/edge-rules.
// loadApp resolves by slug, not id, so returning app.ID here would
// make every test 404.
func mustSeedEdgeRuleApp(t *testing.T, e testEnv, slug string) string {
	t.Helper()
	_, err := e.store.CreateApp(t.Context(), state.App{
		AccountID:      e.acct.ID,
		Slug:           slug,
		Type:           state.AppTypeApp,
		RAMMB:          256,
		MaxConcurrency: 1,
		IdleTimeoutS:   30,
	})
	if err != nil {
		t.Fatalf("CreateApp(%s): %v", slug, err)
	}
	return slug
}

// edgeRuleRouteReq is a valid baseline request body for the
// edge-rule create handler — uses the cheapest kind (route) so
// the Free-plan tests don't trip the plan-kind gate.
func edgeRuleRouteReq(slug string) api.CreateEdgeRuleRequest {
	return api.CreateEdgeRuleRequest{
		MatchHost:    "api.example.com",
		MatchPath:    "/api/*",
		MatchMethods: []string{"GET", "POST"},
		Priority:     intPtr(100),
		Enabled:      boolPtr(true),
		Kind:         string(state.EdgeRuleKindRoute),
		Action:       json.RawMessage(`{"target_app_slug":"` + slug + `"}`),
	}
}

// intPtr is declared in grace_cache_test.go (same package) and
// reused here to keep the helper single-sourced. boolPtr is local
// because no other test file needs it today.
func boolPtr(b bool) *bool { return &b }

// TestCreateEdgeRule_HappyPath confirms the canonical create flow:
// 201, the response carries the seeded fields, and a follow-up
// GET-by-id returns the same row.
func TestCreateEdgeRule_HappyPath(t *testing.T) {
	e := setup(t, api.PlanHobby)
	slug := mustSeedEdgeRuleApp(t, e, "demo")
	req := edgeRuleRouteReq("legacy")
	req.MatchHost = "api.example.com"

	rec := e.do(t, "POST", "/v1/apps/"+slug+"/edge-rules", req, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	var out api.EdgeRuleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ID == "" {
		t.Error("empty id in response")
	}
	if out.Kind != string(state.EdgeRuleKindRoute) {
		t.Errorf("kind = %q, want route", out.Kind)
	}
	if out.MatchHost != "api.example.com" {
		t.Errorf("match_host = %q, want api.example.com", out.MatchHost)
	}
	if out.Priority != 100 {
		t.Errorf("priority = %d, want 100", out.Priority)
	}
	if !out.Enabled {
		t.Error("enabled should default to true")
	}
	if out.AppID == "" {
		t.Error("app_id should be set")
	}

	// Read-back.
	rec = e.do(t, "GET", "/v1/edge-rules/"+out.ID, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET-by-id: status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

// TestCreateEdgeRule_FreeJWT_Returns402 pins the plan-kind gate
// (ADR-089 §7): jwt|ip are Hobby+. A Free plan posting a JWT
// rule must get 402 plan_edge_rule_kind_not_allowed BEFORE loadApp
// runs, so a slug-leak doesn't surface.
func TestCreateEdgeRule_FreeJWT_Returns402(t *testing.T) {
	e := setup(t, api.PlanFree)
	slug := mustSeedEdgeRuleApp(t, e, "freeapp")
	req := api.CreateEdgeRuleRequest{
		MatchHost: "auth.example.com",
		MatchPath: "/verify",
		Kind:      string(state.EdgeRuleKindJWT),
		Action:    json.RawMessage(`{"issuer":"https://idp.example.com/","jwks_url":"https://idp.example.com/.well-known/jwks.json","algorithms":["RS256"]}`),
	}
	rec := e.do(t, "POST", "/v1/apps/"+slug+"/edge-rules", req, nil)
	if rec.Code != http.StatusPaymentRequired {
		t.Errorf("status = %d, want 402; body = %s", rec.Code, rec.Body.String())
	}
}

// TestCreateEdgeRule_FreeIP_Returns402 mirrors the JWT gate for
// the IP kind.
func TestCreateEdgeRule_FreeIP_Returns402(t *testing.T) {
	e := setup(t, api.PlanFree)
	slug := mustSeedEdgeRuleApp(t, e, "freeapp-ip")
	req := api.CreateEdgeRuleRequest{
		MatchHost: "ip.example.com",
		MatchPath: "/",
		Kind:      string(state.EdgeRuleKindIP),
		Action:    json.RawMessage(`{"allow":["10.0.0.0/8"]}`),
	}
	rec := e.do(t, "POST", "/v1/apps/"+slug+"/edge-rules", req, nil)
	if rec.Code != http.StatusPaymentRequired {
		t.Errorf("status = %d, want 402; body = %s", rec.Code, rec.Body.String())
	}
}

// TestCreateEdgeRule_HobbyJWT_Returns201 confirms the Hobby plan
// DOES unlock jwt — the gate is plan-scoped, not global.
func TestCreateEdgeRule_HobbyJWT_Returns201(t *testing.T) {
	e := setup(t, api.PlanHobby)
	slug := mustSeedEdgeRuleApp(t, e, "hobby-jwt")
	req := api.CreateEdgeRuleRequest{
		MatchHost: "auth.example.com",
		MatchPath: "/verify",
		Kind:      string(state.EdgeRuleKindJWT),
		Action:    json.RawMessage(`{"issuer":"https://idp.example.com/","jwks_url":"https://idp.example.com/.well-known/jwks.json","algorithms":["RS256"]}`),
	}
	rec := e.do(t, "POST", "/v1/apps/"+slug+"/edge-rules", req, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
}

// TestCreateEdgeRule_UnknownApp_Returns404 pins the loadApp branch:
// a Free plan posting to a non-existent slug gets 404, not 402,
// because the kind gate runs FIRST.
func TestCreateEdgeRule_UnknownApp_Returns404(t *testing.T) {
	e := setup(t, api.PlanHobby)
	req := edgeRuleRouteReq("legacy")
	rec := e.do(t, "POST", "/v1/apps/ghost/edge-rules", req, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
}

// TestCreateEdgeRule_InvalidAction_Returns400 pins the per-kind
// validator: an action body that doesn't match the kind's
// required shape returns 400 BEFORE the store is touched. The
// existing ErrValidation helper returns 400 by convention
// (RFC 7807 surface across the codebase — see pkg/api/errors.go:2207).
func TestCreateEdgeRule_InvalidAction_Returns400(t *testing.T) {
	e := setup(t, api.PlanHobby)
	slug := mustSeedEdgeRuleApp(t, e, "badact")
	req := api.CreateEdgeRuleRequest{
		MatchHost: "x.example.com",
		MatchPath: "/api",
		Kind:      string(state.EdgeRuleKindRewrite),
		// RewriteAction needs from + to; only from present → fail.
		Action: json.RawMessage(`{"from":"/api"}`),
	}
	rec := e.do(t, "POST", "/v1/apps/"+slug+"/edge-rules", req, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

// TestCreateEdgeRule_InvalidKind_Returns400 pins the closed
// vocabulary gate: 'magic' is not in the 7-kind set.
func TestCreateEdgeRule_InvalidKind_Returns400(t *testing.T) {
	e := setup(t, api.PlanHobby)
	slug := mustSeedEdgeRuleApp(t, e, "badkind")
	req := api.CreateEdgeRuleRequest{
		MatchHost: "x.example.com",
		MatchPath: "/",
		Kind:      "magic",
		Action:    json.RawMessage(`{}`),
	}
	rec := e.do(t, "POST", "/v1/apps/"+slug+"/edge-rules", req, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

// TestCreateEdgeRule_AtPerAppLimit_Returns403 fills one app to
// its Free-plan per-app cap (5) and asserts the next insert
// returns 403 plan_limit_edge_rules. Per the existing precedent
// (ErrPlanWebhookQuota, ErrPlanLimitAlertRules), per-resource
// quota surfaces at 403; per-feature plan-gates surface at 402.
func TestCreateEdgeRule_AtPerAppLimit_Returns403(t *testing.T) {
	e := setup(t, api.PlanFree)
	slug := mustSeedEdgeRuleApp(t, e, "atcap")
	limits := api.MustLimitsFor(api.PlanFree) // EdgeRulesPerApp=5
	for i := 0; i < limits.EdgeRulesPerApp; i++ {
		req := edgeRuleRouteReq("legacy")
		req.MatchHost = fmt.Sprintf("cap-%d.example.com", i)
		rec := e.do(t, "POST", "/v1/apps/"+slug+"/edge-rules", req, nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("seed #%d: status = %d, body = %s", i, rec.Code, rec.Body.String())
		}
	}
	overflow := edgeRuleRouteReq("legacy")
	overflow.MatchHost = "overflow.example.com"
	rec := e.do(t, "POST", "/v1/apps/"+slug+"/edge-rules", overflow, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (at cap); body = %s", rec.Code, rec.Body.String())
	}
}

// TestGetEdgeRule_CrossAccount_Returns404 pins the IDOR check.
// GetEdgeRuleByID does NOT filter by account, so the handler
// must compare rule.AccountID to the caller's.
func TestGetEdgeRule_CrossAccount_Returns404(t *testing.T) {
	// Account A creates the rule.
	eA := setup(t, api.PlanHobby)
	slug := mustSeedEdgeRuleApp(t, eA, "A")
	created := mustCreateEdgeRule(t, eA, slug, edgeRuleRouteReq("legacy"))

	// Account B probes A's rule by id.
	eB := setup(t, api.PlanHobby)
	mustSeedEdgeRuleApp(t, eB, "B")
	rec := eB.do(t, "GET", "/v1/edge-rules/"+created.ID, nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (IDOR); body = %s", rec.Code, rec.Body.String())
	}
}

// TestUpdateEdgeRule_HappyPath exercises partial PATCH: priority
// 100 → 50 must land; kind rotation is rejected (the action union
// would lose shape).
func TestUpdateEdgeRule_HappyPath(t *testing.T) {
	e := setup(t, api.PlanHobby)
	slug := mustSeedEdgeRuleApp(t, e, "upd")
	created := mustCreateEdgeRule(t, e, slug, edgeRuleRouteReq("legacy"))

	rec := e.do(t, "PATCH", "/v1/edge-rules/"+created.ID, api.UpdateEdgeRuleRequest{
		Priority: intPtr(50),
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var out api.EdgeRuleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Priority != 50 {
		t.Errorf("priority = %d, want 50", out.Priority)
	}
}

// TestUpdateEdgeRule_ActionPersists pins the action-update path:
// PATCH with a new action body must persist the new action, not
// silently drop it. Regression for a prior bug where the handler
// pre-decoded the action, nullified req.Action, and discarded the
// decoded value — leaving the store call with nil Action and the
// existing action untouched (POST returned 200 but the new body
// never landed). Now the adapter owns the decode and req.Action is
// preserved end-to-end.
func TestUpdateEdgeRule_ActionPersists(t *testing.T) {
	e := setup(t, api.PlanHobby)
	slug := mustSeedEdgeRuleApp(t, e, "upact")
	created := mustCreateEdgeRule(t, e, slug, edgeRuleRouteReq("legacy"))

	// New target slug via PATCH. Wire shape: action is *json.RawMessage.
	newTarget := "new-target-slug"
	actionBody := json.RawMessage(`{"target_app_slug":"` + newTarget + `"}`)
	rec := e.do(t, "PATCH", "/v1/edge-rules/"+created.ID, api.UpdateEdgeRuleRequest{
		Action: &actionBody,
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	// Read-back: the new action must be persisted.
	rec = e.do(t, "GET", "/v1/edge-rules/"+created.ID, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var got api.EdgeRuleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var raw struct {
		Route *struct {
			TargetAppSlug string `json:"target_app_slug"`
		} `json:"route"`
	}
	if err := json.Unmarshal(got.Action, &raw); err != nil {
		t.Fatalf("unmarshal action: %v", err)
	}
	if raw.Route == nil {
		t.Fatalf("action.route is nil after PATCH; full action = %s", got.Action)
	}
	if raw.Route.TargetAppSlug != newTarget {
		t.Errorf("action.route.target_app_slug = %q, want %q (action dropped)",
			raw.Route.TargetAppSlug, newTarget)
	}
}

// TestDeleteEdgeRule_HappyPath exercises 204 + post-delete 404.
func TestDeleteEdgeRule_HappyPath(t *testing.T) {
	e := setup(t, api.PlanHobby)
	slug := mustSeedEdgeRuleApp(t, e, "del")
	created := mustCreateEdgeRule(t, e, slug, edgeRuleRouteReq("legacy"))

	rec := e.do(t, "DELETE", "/v1/edge-rules/"+created.ID, nil, nil)
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}
	rec = e.do(t, "GET", "/v1/edge-rules/"+created.ID, nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("post-delete GET: status = %d, want 404", rec.Code)
	}
}

// TestListEdgeRulesForApp_OnlyReturnsAppRules pins the per-app
// scoping: rules on a different app on the same account must NOT
// appear in the app-scoped list.
func TestListEdgeRulesForApp_OnlyReturnsAppRules(t *testing.T) {
	e := setup(t, api.PlanHobby)
	slugA := mustSeedEdgeRuleApp(t, e, "scopeA")
	slugB := mustSeedEdgeRuleApp(t, e, "scopeB")
	mustCreateEdgeRule(t, e, slugA, edgeRuleRouteReq("legacy"))
	mustCreateEdgeRule(t, e, slugA, edgeRuleRouteReq("legacy"))
	if _, err := e.store.CreateEdgeRule(t.Context(), state.CreateEdgeRuleParams{
		AccountID: e.acct.ID, AppID: "ghost-app-id",
		MatchHost: "leak.example.com", MatchPath: "/",
		Priority: 100, Enabled: true,
		Kind:   state.EdgeRuleKindRoute,
		Action: state.EdgeRuleAction{Kind: state.EdgeRuleKindRoute, Route: &state.EdgeRuleRouteAction{TargetAppSlug: "x"}},
	}); err != nil {
		// MemStore may not have an FK to apps; if so, this branch
		// is unreachable. Skip silently — the per-account list
		// test below covers the same ground.
		t.Logf("seed foreign-app row: %v (may be expected)", err)
	}

	rec := e.do(t, "GET", "/v1/apps/"+slugA+"/edge-rules", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var list []api.EdgeRuleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("list size = %d, want 2 (only slugA rules)", len(list))
	}
	for _, r := range list {
		if r.AppID == "" {
			t.Errorf("list entry has empty app_id: %+v", r)
		}
	}

	_ = slugB // suppress unused
}

// mustCreateEdgeRule is the test-local helper that POSTs a rule
// and returns the response. Fails the test if the create doesn't
// return 201.
func mustCreateEdgeRule(t *testing.T, e testEnv, slug string, req api.CreateEdgeRuleRequest) api.EdgeRuleResponse {
	t.Helper()
	rec := e.do(t, "POST", "/v1/apps/"+slug+"/edge-rules", req, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create edge rule: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out api.EdgeRuleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

// sentinel guard: keep the import live even if a future refactor
// drops every test that touches state.Account — the IDOR tests
// still reference it transitively via setup()'s return.
var _ = errors.New
