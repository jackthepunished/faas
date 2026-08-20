package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// TestGetDomainDoctor_DisabledFlagReturns503 (ADR-120 Tier A3)
// asserts that an operator opt-out (FAAS_DOMAIN_DOCTOR_ENABLED=false)
// makes the doctor route return 503 doctor_disabled even though
// the domain exists in the store. The handler's first-line guard
// at cmd/apid/handlers_ext.go:1892 short-circuits before any
// probe logic so a single env flip is the only knob needed to
// disable the doctor fleet-wide.
func TestGetDomainDoctor_DisabledFlagReturns503(t *testing.T) {
	withDomainDoctorDisabled(t)
	e := setup(t, api.PlanPro)
	appID := mustSeedApp(t, e, "doc-flag-app")
	if _, err := e.store.CreateCustomDomain(context.Background(), "flag.example.com", appID, "tok"); err != nil {
		t.Fatalf("seed domain: %v", err)
	}
	rec := e.do(t, "GET", "/v1/domains/flag.example.com/doctor", nil, nil)
	assertProblem(t, rec, http.StatusServiceUnavailable, api.CodeDoctorDisabled)
}

// TestGetDomainDoctor_FlagEnabledServesReport (ADR-120 Tier A3)
// asserts the happy path: flag on, domain seeded, an observation
// row exists, the JSON response matches the wire DTO shape with
// the 5-check report. The 5-row invariant is load-bearing per
// pkg/api/dto.go — adding a new check there requires updating
// the dashboard rendering AND the Alertmanager runbook so the
// three surfaces stay in lockstep.
func TestGetDomainDoctor_FlagEnabledServesReport(t *testing.T) {
	withDomainDoctorEnabled(t)
	e := setup(t, api.PlanPro)
	appID := mustSeedApp(t, e, "doc-on-app")
	if _, err := e.store.CreateCustomDomain(context.Background(), "on.example.com", appID, "tok"); err != nil {
		t.Fatalf("seed domain: %v", err)
	}
	// Seed a fully-OK observation row so buildDoctorReport returns
	// without triggering a synchronous re-probe (which would dial
	// a real port-443 and break the unit test).
	obs := state.DomainDoctorObservation{
		Domain:          "on.example.com",
		ObservedAt:      time.Now().UTC(),
		DNSRecordFound:  true,
		PointsToGregale: true,
		IPv6Conflict:    false,
		CertState:       "issued",
		ObservedTarget:  "apps.gregale.dev",
		DNSCheckedAt:    time.Now().UTC(),
	}
	if err := e.store.UpsertDoctorObservation(context.Background(), obs); err != nil {
		t.Fatalf("upsert obs: %v", err)
	}
	rec := e.do(t, "GET", "/v1/domains/on.example.com/doctor", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var report api.DomainDoctorReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if report.Domain != "on.example.com" {
		t.Errorf("domain = %q; want on.example.com", report.Domain)
	}
	if len(report.Checks) != 5 {
		t.Errorf("checks len = %d; want 5 (DNS / points_to_gregale / TLS / CAA / IPv6)", len(report.Checks))
	}
	if !report.Healthy {
		t.Errorf("Healthy = false; want true (every check ok in the seeded obs)")
	}
}

// TestGetDomainDoctor_IDOR (ADR-120 Tier A2) asserts a cross-
// tenant probe returns 404 (not 403 — no leak that the row
// exists). The handler's loadDomain helper at
// cmd/apid/handlers_ext.go:1722 owns the ownership check and
// short-circuits with ok=false on cross-tenant; the wrapper
// at handlers_ext.go:1892 returns 404 not_found when loadDomain
// signals failure.
func TestGetDomainDoctor_IDOR(t *testing.T) {
	withDomainDoctorEnabled(t)
	// Account A creates the domain.
	a := setup(t, api.PlanPro)
	appIDA := mustSeedApp(t, a, "idor-a")
	if _, err := a.store.CreateCustomDomain(context.Background(), "shared.example.com", appIDA, "tok"); err != nil {
		t.Fatalf("seed domain A: %v", err)
	}
	// Account B probes it.
	b := setup(t, api.PlanPro)
	if _, err := b.store.CreateCustomDomain(context.Background(), "victim.example.com", appIDA, "tok"); err != nil {
		t.Fatalf("seed victim: %v", err)
	}
	// Account B's sessionAuth must NOT see account A's domain.
	rec := b.do(t, "GET", "/v1/domains/shared.example.com/doctor", nil, nil)
	assertProblem(t, rec, http.StatusNotFound, api.CodeNotFound)
}

// TestGetDomainDoctor_StaleFlagFlipsResponse (ADR-120 Tier A3)
// asserts that an observation row older than FAAS_DOMAIN_DOCTOR_TTL_SECONDS
// surfaces in the response as Stale=true. Mirrors the dashboard's
// stale-banner rendering so the JSON + HTML stay in lockstep.
func TestGetDomainDoctor_StaleFlagFlipsResponse(t *testing.T) {
	withDomainDoctorEnabled(t)
	// Force the TTL to 5s so we can build a stale row deterministically.
	t.Setenv("FAAS_DOMAIN_DOCTOR_TTL_SECONDS", "5")
	e := setup(t, api.PlanPro)
	appID := mustSeedApp(t, e, "stale-app")
	if _, err := e.store.CreateCustomDomain(context.Background(), "stale.example.com", appID, "tok"); err != nil {
		t.Fatalf("seed domain: %v", err)
	}
	// Seed an observation row 30s in the past — well past the 5s TTL.
	obs := state.DomainDoctorObservation{
		Domain:          "stale.example.com",
		ObservedAt:      time.Now().UTC().Add(-30 * time.Second),
		DNSRecordFound:  true,
		PointsToGregale: true,
		IPv6Conflict:    false,
		CertState:       "issued",
	}
	if err := e.store.UpsertDoctorObservation(context.Background(), obs); err != nil {
		t.Fatalf("upsert obs: %v", err)
	}
	rec := e.do(t, "GET", "/v1/domains/stale.example.com/doctor", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var report api.DomainDoctorReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !report.Stale {
		t.Errorf("Stale = false; want true (TTL=5s, observation 30s old)")
	}
}

// TestParseDomainDoctorPath (ADR-120 Tier A2) covers the dashboard
// route parser's three observable states: matching
// {slug}/domains/{domain}/doctor → (slug, domain, true);
// non-matching suffix → ok=false (caller falls through to
// renderAppDetail); empty domain or empty slug → ok=false
// (defensive — same posture as parseDeployDetailPath).
func TestParseDomainDoctorPath(t *testing.T) {
	cases := []struct {
		in         string
		wantSlug   string
		wantDomain string
		wantOK     bool
	}{
		{"api-app/domains/api.example.com/doctor", "api-app", "api.example.com", true},
		{"app-1/domains/sub.example.com/doctor", "app-1", "sub.example.com", true},
		// No /domains/ prefix — falls through.
		{"app-1/deployments/dep-1", "", "", false},
		// Empty domain — defensive fail.
		{"app-1/domains//doctor", "", "", false},
		// Missing /doctor suffix — falls through.
		{"app-1/domains/api.example.com", "", "", false},
		// Empty slug — defensive fail.
		{"/domains/api.example.com/doctor", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			gotSlug, gotDomain, gotOK := parseDomainDoctorPath(tc.in)
			if gotSlug != tc.wantSlug || gotDomain != tc.wantDomain || gotOK != tc.wantOK {
				t.Fatalf("parseDomainDoctorPath(%q) = (%q, %q, %v); want (%q, %q, %v)",
					tc.in, gotSlug, gotDomain, gotOK, tc.wantSlug, tc.wantDomain, tc.wantOK)
			}
		})
	}
}

// TestRenderDomainDoctor_RouteRegistered (ADR-120 Tier A2)
// asserts the dashboard HTTP path
// /dashboard/apps/{slug}/domains/{domain}/doctor reaches the
// renderDomainDoctor handler and returns 200 with the doctor
// template body. Uses newAuthedDashboardServerFull so the
// sessionAuth middleware sees a real faas_sid cookie (the
// /dashboard/* mount at server.go:1670 short-circuits to
// /login otherwise).
func TestRenderDomainDoctor_RouteRegistered(t *testing.T) {
	withDomainDoctorEnabled(t)
	h, cookie, store, _ := newAuthedDashboardServerFull(t)
	acct, err := store.AccountByEmail(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("seed account: %v", err)
	}
	app, err := store.CreateApp(context.Background(), state.App{
		AccountID: acct.ID, Slug: "render-doc-app",
	})
	if err != nil {
		t.Fatalf("seed app: %v", err)
	}
	if _, err := store.CreateCustomDomain(context.Background(), "render.example.com", app.ID, "tok"); err != nil {
		t.Fatalf("seed domain: %v", err)
	}
	obs := state.DomainDoctorObservation{
		Domain:          "render.example.com",
		ObservedAt:      time.Now().UTC(),
		DNSRecordFound:  true,
		PointsToGregale: true,
		IPv6Conflict:    false,
		CertState:       "issued",
	}
	if err := store.UpsertDoctorObservation(context.Background(), obs); err != nil {
		t.Fatalf("upsert obs: %v", err)
	}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet,
		"/dashboard/apps/render-doc-app/domains/render.example.com/doctor", nil)
	r.AddCookie(cookie)
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "render.example.com") {
		t.Errorf("body should mention domain; got %q", body)
	}
	if !strings.Contains(body, "Checks") {
		t.Errorf("body should render the Checks h2; got %q", body)
	}
}
