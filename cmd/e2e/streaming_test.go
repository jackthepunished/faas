// streaming_test.go — issue #471 PR-A acceptance for the *flag +
// plan-gate* surface (AC #4 splits cleanly: the e2e pin below
// covers the apid side; the gateway-side buffered-fallback log line
// is pinned in pkg/gateway/handler_test.go in-process — see note
// below). PR-A wires the per-app streaming flag, plan default, env
// flag, and the buffered-fallback deprecation log; the real Flusher
// path ships in PR-B. This test exercises three pins, all on the
// apid + Postgres surface:
//
//  1. Plan-gate: a Free-plan customer cannot enable streaming.
//     PATCH /v1/apps/{slug} with streaming_enabled=true returns
//     403 plan_streaming_not_allowed with the structured error
//     envelope (limit value, observed value, docs URL).
//
//  2. Plan-default: a Hobby customer creates an app → the persisted
//     App.streaming_enabled matches the plan's default (true for
//     Hobby+; PR-A flips the same path PR-B uses).
//
//  3. Persistence: a Hobby customer can enable streaming explicitly
//     via PATCH and the row reflects the new value.
//
// What this test does NOT cover (deliberately): the buffered-fallback
// deprecation log itself. That log line is emitted inside gatewayd's
// statusRecorder post-proxy branch when an SSE response arrives on
// the legacy buffered path; it is a sync.Map dedup + a slog.Warn
// with no Postgres involvement. Asserting it here would mean
// standing up gatewayd in the harness, deploying an SSE-emitting app,
// and tailing slog — far more surface than the unit test in
// pkg/gateway/handler_test.go already covers. The end-to-end "real
// SSE response reaching gatewayd and being buffered" path is a
// PR-B metal test (the e2e harness without a deployed app can't
// synthesize a successful 200 with an SSE Content-Type).
//
// Build tag: (none). CI-safe. Requires Postgres (skip via
// FAAS_SKIP_PG_TESTS) and a buildable ./cmd/apid.

package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
	"github.com/onebox-faas/faas/pkg/e2etest"
)

// TestE2E_Streaming_FreePlanRejected covers AC #4 sub-test (1):
// a Free-plan account cannot flip streaming_enabled=true on an
// app. apid's validateUpdateApp plan-gate emits 403
// plan_streaming_not_allowed with a structured Problem body that
// carries the limit value (the plan's StreamingResponseAllowed
// predicate), the observed value (true), and the docs URL.
func TestE2E_Streaming_FreePlanRejected(t *testing.T) {
	pool := pgtest.Open(t)
	if pool == nil {
		return
	}
	if err := db.MigrateUp(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h := e2etest.Start(t, pool, e2etest.APID)
	key := h.SeedAccount(context.Background(), api.PlanFree)

	// Read the limits so the test stays in sync if the Free plan
	// ever unlocks streaming. Today (PR-A) Free is off — the
	// assertion below fails loudly the moment a future change
	// silently flips the default, exactly the regression the AC
	// is meant to catch.
	if api.PlanFree.StreamingResponseAllowed() {
		t.Fatalf("PlanFree reported StreamingResponseAllowed=true; AC #4 expects Free to be blocked")
	}

	// Seed one app on the Free plan.
	createBody, status := doReq(t, h, key, http.MethodPost, "/v1/apps",
		api.CreateAppRequest{Slug: "stream-free"})
	if status != http.StatusCreated {
		t.Fatalf("create app: %d %s", status, createBody)
	}

	// Attempt to flip streaming on — must be rejected.
	trueVal := true
	raw, status := doReq(t, h, key, http.MethodPatch, "/v1/apps/stream-free",
		api.UpdateAppRequest{StreamingEnabled: &trueVal})
	if status != http.StatusForbidden {
		t.Fatalf("PATCH streaming_enabled=true on Free plan: status=%d want %d body=%s",
			status, http.StatusForbidden, raw)
	}
	var prob api.Problem
	if err := json.Unmarshal(raw, &prob); err != nil {
		t.Fatalf("decode problem body: %v (raw=%s)", err, raw)
	}
	if prob.Code != api.CodePlanStreamingNotAllowed {
		t.Errorf("prob.code = %q, want %q", prob.Code, api.CodePlanStreamingNotAllowed)
	}
	if prob.Status != http.StatusForbidden {
		t.Errorf("prob.status = %d, want %d", prob.Status, http.StatusForbidden)
	}
}

// TestE2E_Streaming_HobbyPlanDefaultsAndPersists covers AC #4
// sub-tests (2) and (3): a Hobby customer creates an app (the
// plan default flows through buildApp → limits.StreamingEnabled
// (Plan)) and then PATCHes streaming_enabled=true (the plan-gate
// passes for Hobby) — both states land on the persisted app.
func TestE2E_Streaming_HobbyPlanDefaultsAndPersists(t *testing.T) {
	pool := pgtest.Open(t)
	if pool == nil {
		return
	}
	if err := db.MigrateUp(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h := e2etest.Start(t, pool, e2etest.APID)
	key := h.SeedAccount(context.Background(), api.PlanHobby)

	if !api.PlanHobby.StreamingResponseAllowed() {
		t.Fatalf("PlanHobby reported StreamingResponseAllowed=false; AC #4 expects Hobby to be allowed")
	}

	// Create — defaults to true (the Hobby+ plan-level default).
	createBody, status := doReq(t, h, key, http.MethodPost, "/v1/apps",
		api.CreateAppRequest{Slug: "stream-hobby"})
	if status != http.StatusCreated {
		t.Fatalf("create app: %d %s", status, createBody)
	}
	var created api.AppResponse
	if err := json.Unmarshal(createBody, &created); err != nil {
		t.Fatalf("decode created: %v (raw=%s)", err, createBody)
	}
	// Diagnostic dump — the CI flake on PR #481 surfaced as
	// "StreamingEnabled=false" with no clear root cause; the raw
	// body pinches the search to either buildApp, the SELECT, or
	// the JSON marshal layer. Cheap (one extra format on every run)
	// and only useful when the assertion trips.
	t.Logf("create response raw=%s planDefault(Hobby)=%v", createBody, api.PlanHobby.StreamingEnabled())
	if !created.StreamingEnabled {
		t.Errorf("Hobby create: StreamingEnabled=false; want true (plan default)")
	}

	// Explicitly disable + re-enable: PATCH must honour the value
	// and the plan-gate must not interfere on Hobby.
	falseVal := false
	raw, status := doReq(t, h, key, http.MethodPatch, "/v1/apps/stream-hobby",
		api.UpdateAppRequest{StreamingEnabled: &falseVal})
	if status != http.StatusOK {
		t.Fatalf("PATCH streaming_enabled=false: %d %s", status, raw)
	}
	var afterOff api.AppResponse
	if err := json.Unmarshal(raw, &afterOff); err != nil {
		t.Fatalf("decode afterOff: %v (raw=%s)", err, raw)
	}
	if afterOff.StreamingEnabled {
		t.Errorf("PATCH off: StreamingEnabled=true; want false")
	}

	trueVal := true
	raw, status = doReq(t, h, key, http.MethodPatch, "/v1/apps/stream-hobby",
		api.UpdateAppRequest{StreamingEnabled: &trueVal})
	if status != http.StatusOK {
		t.Fatalf("PATCH streaming_enabled=true: %d %s", status, raw)
	}
	var afterOn api.AppResponse
	if err := json.Unmarshal(raw, &afterOn); err != nil {
		t.Fatalf("decode afterOn: %v (raw=%s)", err, raw)
	}
	if !afterOn.StreamingEnabled {
		t.Errorf("PATCH on: StreamingEnabled=false; want true")
	}

	// Read back via GET to confirm the persisted row matches.
	raw, status = doReq(t, h, key, http.MethodGet, "/v1/apps/stream-hobby", nil)
	if status != http.StatusOK {
		t.Fatalf("GET app: %d %s", status, raw)
	}
	var fetched api.AppResponse
	if err := json.Unmarshal(raw, &fetched); err != nil {
		t.Fatalf("decode fetched: %v (raw=%s)", err, raw)
	}
	if !fetched.StreamingEnabled {
		t.Errorf("GET after PATCH on: StreamingEnabled=false; want true (persisted)")
	}
}
