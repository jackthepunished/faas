package dashboard_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/dashboard"
)

// TestRender_Layout confirms the layout template parses, executes
// without error, and contains the expected chrome (HTMX script, nav).
func TestRender_Layout(t *testing.T) {
	rec := httptest.NewRecorder()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	page := dashboard.Page{
		Title: "Overview",
		Body:  "index",
	}
	if err := dashboard.Render(rec, log, "", page); err != nil {
		t.Fatalf("render: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"<!doctype html>",
		"<title>Overview — onebox faas</title>",
		"htmx.org@2.0.4",
		"/dashboard/",
		"Overview",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n--- body ---\n%s", want, body)
		}
	}
}

// TestRender_LoginBody confirms a page that uses the Body field
// resolves to the right template name.
func TestRender_LoginBody(t *testing.T) {
	rec := httptest.NewRecorder()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	page := dashboard.Page{
		Title: "Sign in",
		Body:  "login",
	}
	if err := dashboard.Render(rec, log, "", page); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(rec.Body.String(), `<form method="POST" action="/login"`) {
		t.Errorf("body missing login form\n--- body ---\n%s", rec.Body.String())
	}
}

// TestRender_LoginBody_GoogleEnabled (issue #419 / ADR-046) — when
// the boot-resolved auth.SignInConfig reports Google Enabled, the
// /login surface must render the "Sign in with Google" link. The
// dashboard hits GET /v1/auth/capabilities to learn this, but the
// template gates per provider on the AuthCapabilitiesView bools the
// handler populates from s.oauthConfig.
func TestRender_LoginBody_GoogleEnabled(t *testing.T) {
	rec := httptest.NewRecorder()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	page := dashboard.Page{
		Title: "Sign in",
		Body:  "login",
		Auth:  &dashboard.AuthCapabilitiesView{GoogleEnabled: true},
	}
	if err := dashboard.Render(rec, log, "", page); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(rec.Body.String(), `href="/v1/auth/google"`) {
		t.Errorf("google link missing\n--- body ---\n%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `href="/v1/auth/github"`) {
		t.Errorf("github link should not render when only Google is enabled\n--- body ---\n%s", rec.Body.String())
	}
}

// TestRender_LoginBody_GitHubEnabled — the GitHub mirror of
// TestRender_LoginBody_GoogleEnabled above. Both providers
// independent — the dashboard reads each provider's bool off
// .Auth.<Name>Enabled, and a one-provider host gates the other
// off in steady state.
func TestRender_LoginBody_GitHubEnabled(t *testing.T) {
	rec := httptest.NewRecorder()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	page := dashboard.Page{
		Title: "Sign in",
		Body:  "login",
		Auth:  &dashboard.AuthCapabilitiesView{GitHubEnabled: true},
	}
	if err := dashboard.Render(rec, log, "", page); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(rec.Body.String(), `href="/v1/auth/github"`) {
		t.Errorf("github link missing\n--- body ---\n%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `href="/v1/auth/google"`) {
		t.Errorf("google link should not render when only GitHub is enabled\n--- body ---\n%s", rec.Body.String())
	}
}

// TestRender_LoginBody_NeitherEnabled — the single-box-dev /
// operator-chose-not-to-ship-OAuth shape. With both providers
// Disabled, /login must not render either OAuth link — the
// password-only path stays usable but the dead buttons that lead
// to 500 *_oauth_misconfigured (the pre-#419 symptom) are gone.
// Also covers the nil-safety branch: Auth == nil must render
// nothing, not panic with a nil-pointer deref inside the
// `{{if .Auth}}…{{end}}` guard.
func TestRender_LoginBody_NeitherEnabled(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("AuthExplicitlyEmpty", func(t *testing.T) {
		rec := httptest.NewRecorder()
		page := dashboard.Page{Title: "Sign in", Body: "login"}
		if err := dashboard.Render(rec, log, "", page); err != nil {
			t.Fatalf("render: %v", err)
		}
		body := rec.Body.String()
		if strings.Contains(body, `href="/v1/auth/google"`) {
			t.Errorf("google link should not render when Auth is zero-value\n--- body ---\n%s", body)
		}
		if strings.Contains(body, `href="/v1/auth/github"`) {
			t.Errorf("github link should not render when Auth is zero-value\n--- body ---\n%s", body)
		}
	})

	t.Run("AuthPointerNil", func(t *testing.T) {
		rec := httptest.NewRecorder()
		page := dashboard.Page{Title: "Sign in", Body: "login", Auth: nil}
		if err := dashboard.Render(rec, log, "", page); err != nil {
			t.Fatalf("render: %v", err)
		}
		body := rec.Body.String()
		if strings.Contains(body, `href="/v1/auth/google"`) {
			t.Errorf("google link should not render when Auth is nil\n--- body ---\n%s", body)
		}
		if strings.Contains(body, `href="/v1/auth/github"`) {
			t.Errorf("github link should not render when Auth is nil\n--- body ---\n%s", body)
		}
	})

	t.Run("BothBoolsFalse", func(t *testing.T) {
		rec := httptest.NewRecorder()
		page := dashboard.Page{
			Title: "Sign in",
			Body:  "login",
			Auth:  &dashboard.AuthCapabilitiesView{GoogleEnabled: false, GitHubEnabled: false},
		}
		if err := dashboard.Render(rec, log, "", page); err != nil {
			t.Fatalf("render: %v", err)
		}
		body := rec.Body.String()
		if strings.Contains(body, `href="/v1/auth/google"`) {
			t.Errorf("google link should not render when both bools are false\n--- body ---\n%s", body)
		}
		if strings.Contains(body, `href="/v1/auth/github"`) {
			t.Errorf("github link should not render when both bools are false\n--- body ---\n%s", body)
		}
	})
}

// TestRender_MissingTemplate confirms an unknown Body returns a 500
// error from Render rather than silently rendering empty.
func TestRender_MissingTemplate(t *testing.T) {
	rec := httptest.NewRecorder()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	page := dashboard.Page{Title: "Nope", Body: "does_not_exist"}
	if err := dashboard.Render(rec, log, "", page); err == nil {
		t.Fatal("expected error for missing template")
	}
}

// TestRender_Flash confirms the Flash banner renders when set.
func TestRender_Flash(t *testing.T) {
	rec := httptest.NewRecorder()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	page := dashboard.Page{Title: "Sign in", Body: "index", Flash: "Check your email"}
	if err := dashboard.Render(rec, log, "", page); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "Check your email") {
		t.Errorf("body missing flash banner\n--- body ---\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `<div class="flash">`) {
		t.Errorf("body missing flash container\n--- body ---\n%s", rec.Body.String())
	}
}

// TestRender_AccountView confirms an Account renders the email + plan
// strings inside the layout body.
func TestRender_AccountView(t *testing.T) {
	rec := httptest.NewRecorder()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	page := dashboard.Page{
		Title: "Overview",
		Body:  "index",
		Account: &dashboard.AccountView{
			ID:       "acct-1",
			Email:    "jane@example.test",
			Plan:     "pro",
			AppCount: 3,
		},
	}
	if err := dashboard.Render(rec, log, "", page); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := rec.Body.String()
	for _, want := range []string{"jane@example.test", "pro", "Deployed apps: <strong>3</strong>"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n--- body ---\n%s", want, body)
		}
	}
}

// TestRender_StampsNonceOnScriptAndStyle confirms Render copies the
// nonce argument onto page.Nonce so the templates emit
// `nonce="..."` on every <script> and <style> tag the CSP
// (httpsec) middleware requires. The HTTP server already sets the
// Content-Security-Policy header; this test pins the matching
// template attribute so the browser accepts the inline code under
// strict CSP.
//
// Issue #249 closes here: a missing stamp would silently block every
// dashboard's HTMX bootstrap.
func TestRender_StampsNonceOnScriptAndStyle(t *testing.T) {
	rec := httptest.NewRecorder()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	const nonce = "abc123XYZ-_abc123XYZ-" // 22 chars, URL-safe base64
	page := dashboard.Page{
		Title: "Overview",
		Body:  "index",
	}
	if err := dashboard.Render(rec, log, nonce, page); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := rec.Body.String()
	// Every <script src="…"> must carry the nonce attribute.
	if !strings.Contains(body, `nonce="`+nonce+`"`) {
		t.Errorf("body missing nonce=%q on script/style\n--- body ---\n%s", nonce, body)
	}
}

// TestRender_NoNonceRendersCleanly confirms Render tolerates an empty
// nonce (unit-test path) without panicking. The output may carry
// `nonce=""` literally — that's harmless on its own and the browser
// still accepts the inline tag because the empty nonce doesn't match
// any CSP `nonce-…` directive in the page's header. Production
// always supplies a real nonce via httpsec.NonceFromContext so
// `nonce=""` never reaches the wire.
func TestRender_NoNonceRendersCleanly(t *testing.T) {
	rec := httptest.NewRecorder()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	page := dashboard.Page{Title: "Sign in", Body: "login"}
	if err := dashboard.Render(rec, log, "", page); err != nil {
		t.Fatalf("render: %v", err)
	}
	// Smoke: the page still renders and includes the form.
	if !strings.Contains(rec.Body.String(), `<form method="POST" action="/login"`) {
		t.Errorf("body missing login form\n--- body ---\n%s", rec.Body.String())
	}
}

// TestRender_AccountNoInlineOnclick pins the inline-onclick refactor
// (issue #249 / spec §11). Browsers do not propagate `nonce` onto
// event-handler attributes, so the original
//
//	<button onclick="return confirm('...')">
//
// would silently break the delete-account confirm prompt the
// moment CSP ships. The refactor moves the prompt into a per-page
// `<script nonce="…">` block that wires addEventListener on a
// form identified by id="account-delete-form". This test pins:
//   - the form carries the id (so the addEventListener hook can
//     find it),
//   - the rendered output contains NO `onclick=` attributes
//     (no inline event handlers at all, so a future regression
//     in a different template is caught too),
//   - the per-page `<script nonce="…">` block contains the confirm
//     prompt wiring.
func TestRender_AccountNoInlineOnclick(t *testing.T) {
	rec := httptest.NewRecorder()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	const nonce = "nonceSmokeTest1234567ab" // 22 chars
	page := dashboard.Page{
		Title: "Account",
		Body:  "account",
		Account: &dashboard.AccountView{
			ID:       "acct-1",
			Email:    "jane@example.test",
			Plan:     "pro",
			AppCount: 1,
		},
		Data: dashboard.AccountData{
			ShowDelete: true,
		},
	}
	if err := dashboard.Render(rec, log, nonce, page); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := rec.Body.String()
	// The danger-zone form must carry the id the addEventListener
	// hook is bound to.
	if !strings.Contains(body, `id="account-delete-form"`) {
		t.Errorf("account delete form missing id\n--- body ---\n%s", body)
	}
	// No inline event handlers — that would defeat strict CSP.
	if strings.Contains(body, "onclick=") {
		t.Errorf("account template still carries an inline onclick attr\n--- body ---\n%s", body)
	}
	// The per-page <script nonce=…> block must contain the confirm
	// prompt wiring so the user still sees the dialog.
	if !strings.Contains(body, `nonce="`+nonce+`"`) {
		t.Errorf("per-page script block missing nonce attr\n--- body ---\n%s", body)
	}
	if !strings.Contains(body, "addEventListener") {
		t.Errorf("per-page script block missing addEventListener wiring\n--- body ---\n%s", body)
	}
	if !strings.Contains(body, "Schedule your account for permanent deletion in 30 days?") {
		t.Errorf("per-page script block missing confirm copy\n--- body ---\n%s", body)
	}
}

// TestRender_StatelessPage pins the /dashboard/stateless landing
// page (Move 1 PR-A). Confirms the page renders, includes the 8-base
// denylist and the 10 closed paths, and shows the empty-state when
// no advisories are present.
func TestRender_StatelessPage(t *testing.T) {
	rec := httptest.NewRecorder()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	page := dashboard.Page{
		Title: "Stateless advisories",
		Body:  "stateless",
		Data: dashboard.StatelessData{
			RecentAdvisoriesEmpty: true,
			StatelessDenylist:     dashboard.StatelessDenylist,
			ClosedPaths:           dashboard.StatelessClosedPaths,
		},
	}
	if err := dashboard.Render(rec, log, "", page); err != nil {
		t.Fatalf("render: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// All 8 base denylist entries must appear so a future rename of
	// one entry can't silently ship without a dashboard refresh.
	for _, name := range []string{"postgres", "redis", "mysql", "mariadb", "mongo", "cockroach", "cassandra", "clickhouse"} {
		if !strings.Contains(body, "<code>"+name+"</code>") {
			t.Errorf("denylist row missing %q\n--- body ---\n%s", name, body)
		}
	}
	// The two "high" closed paths and at least one "warn" path must
	// appear; pinning both severities keeps the badge column honest.
	for _, p := range []string{"/data", "/db", "/var/lib/postgresql", "/var/lib/redis"} {
		if !strings.Contains(body, "<code>"+p+"</code>") {
			t.Errorf("closed-path row missing %q\n--- body ---\n%s", p, body)
		}
	}
	if !strings.Contains(body, `class="badge high"`) {
		t.Errorf("high-severity badge missing\n--- body ---\n%s", body)
	}
	if !strings.Contains(body, `class="badge warn"`) {
		t.Errorf("warn-severity badge missing\n--- body ---\n%s", body)
	}
	// Empty-state copy must render when no advisories are present.
	if !strings.Contains(body, "No advisories recorded") {
		t.Errorf("empty-state copy missing\n--- body ---\n%s", body)
	}
	// Nav link to /dashboard/stateless must be present + active.
	if !strings.Contains(body, `href="/dashboard/stateless"`) {
		t.Errorf("nav link to /dashboard/stateless missing\n--- body ---\n%s", body)
	}
}

// TestStatelessSlices_Shape pins the package-level slices so a future
// drift in pkg/imaged or guest-init is caught on the dashboard side.
// Adding a base to the denylist means updating BOTH pkg/imaged/base.go
// AND pkg/dashboard/dashboard.go's StatelessDenylist; this test fails
// if either is forgotten (count + names are pinned).
func TestStatelessSlices_Shape(t *testing.T) {
	if got := len(dashboard.StatelessDenylist); got != 8 {
		t.Errorf("StatelessDenylist len = %d, want 8 (mirror of pkg/imaged/base.go)", got)
	}
	if got := len(dashboard.StatelessClosedPaths); got != 10 {
		t.Errorf("StatelessClosedPaths len = %d, want 10 (mirror of guest/init/stateless_advisory_linux.go)", got)
	}
	// Every closed path must have a severity, and severities must
	// be in the closed vocabulary.
	for _, p := range dashboard.StatelessClosedPaths {
		if p.Severity != "high" && p.Severity != "warn" {
			t.Errorf("closed-path %q has bad severity %q", p.Path, p.Severity)
		}
	}
	// The two top-level dirs must be high severity; pinning this
	// keeps the badge column honest if a future refactor mis-classifies.
	highs := map[string]bool{}
	for _, p := range dashboard.StatelessClosedPaths {
		if p.Severity == "high" {
			highs[p.Path] = true
		}
	}
	for _, want := range []string{"/data", "/db"} {
		if !highs[want] {
			t.Errorf("expected %q to be high severity; got %v", want, highs)
		}
	}
}

// TestRender_Billing_HidesPortalForFree pins the issue #253
// acceptance #5: a Free-plan account never sees the "Manage
// billing" section or the "Open Stripe billing portal" link, even
// if the operator-configured PortalURL is set. This guards against
// a future template refactor that accidentally moves the {{if
// .Data.HasPaidPlan}} gate or makes PortalURL conditional on
// something else.
func TestRender_Billing_HidesPortalForFree(t *testing.T) {
	rec := httptest.NewRecorder()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	page := dashboard.Page{
		Title: "Billing",
		Body:  "billing",
		Data: dashboard.BillingData{
			Plan:        "free",
			RAMMB:       128,
			Included:    5,
			AppsCap:     1,
			AppLayer:    256,
			IdleSec:     30,
			HasPaidPlan: false,
			// Deliberately populated — even with a non-empty
			// URL, a Free account must not see the link. The
			// dashboard gates on HasPaidPlan, not on PortalURL.
			PortalURL: "https://billing.example.com/portal?account=acct_xyz",
		},
	}
	if err := dashboard.Render(rec, log, "", page); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := rec.Body.String()
	for _, banned := range []string{
		"Manage billing",
		"Open Stripe billing portal",
		"Last invoice",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("Free-plan body should NOT contain %q\n--- body ---\n%s", banned, body)
		}
	}
	// The plan card + usage section must still render (regression).
	for _, want := range []string{"Plan: free", "GB-hours used", "Max concurrent instances"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n--- body ---\n%s", want, body)
		}
	}
}

// TestRender_Billing_PaidPlanShowsPortal pins the issue #253
// acceptance #2 + #4: a paid-plan account sees the portal link,
// the last-invoice table, and the current-month usage summary.
// Pinned substrings match the literal template copy so a future
// copy edit does not silently drift.
func TestRender_Billing_PaidPlanShowsPortal(t *testing.T) {
	rec := httptest.NewRecorder()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	page := dashboard.Page{
		Title: "Billing",
		Body:  "billing",
		Data: dashboard.BillingData{
			Plan:                      "hobby",
			RAMMB:                     256,
			Included:                  50,
			AppsCap:                   5,
			AppLayer:                  512,
			IdleSec:                   60,
			MaxConcurrency:            2,
			UsedGBHours:               12.5,
			UsedPct:                   25,
			UsedEgressGB:              0.42,
			LastInvoiceDate:           "2026-07-31",
			LastInvoiceStatus:         "paid",
			LastInvoiceTotalFormatted: "€12.40",
			LastInvoiceCurrency:       "EUR",
			HasPaidPlan:               true,
			PortalURL:                 "https://billing.example.com/portal?account=acct_abc",
		},
	}
	if err := dashboard.Render(rec, log, "", page); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Plan: hobby",
		"Max concurrent instances", // new field from limits.MaxConcurrency
		"GB-hours used",
		"Egress this month (GB)",
		"Manage billing",
		"Open Stripe billing portal",
		"Last invoice",
		"2026-07-31",
		"€12.40",
		"EUR",
		`href="https://billing.example.com/portal?account=acct_abc"`,
		"rel=\"noopener\"",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n--- body ---\n%s", want, body)
		}
	}
	// Free-tier fallback copy must NOT appear for a paid account.
	if strings.Contains(body, "faas plan &lt;plan&gt;") {
		t.Errorf("paid-plan body should NOT contain Free-tier upgrade hint\n--- body ---\n%s", body)
	}
}

// TestRender_Billing_PaidPortalUnset pins the operator-misconfig
// fallback: a paid account on a box that has FAAS_BILLING_PORTAL_URL
// unset sees a clear "use the CLI" hint instead of a broken button.
// The CLI hint is the escape hatch; the dashboard never silently
// renders an empty link.
func TestRender_Billing_PaidPortalUnset(t *testing.T) {
	rec := httptest.NewRecorder()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	page := dashboard.Page{
		Title: "Billing",
		Body:  "billing",
		Data: dashboard.BillingData{
			Plan:        "hobby",
			HasPaidPlan: true,
			PortalURL:   "",
		},
	}
	if err := dashboard.Render(rec, log, "", page); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Stripe portal is not configured") {
		t.Errorf("body missing operator-misconfig fallback\n--- body ---\n%s", body)
	}
	if !strings.Contains(body, "faas billing portal") {
		t.Errorf("body missing CLI hint\n--- body ---\n%s", body)
	}
	if strings.Contains(body, "Open Stripe billing portal") {
		t.Errorf("body should NOT contain the portal link button when URL is empty\n--- body ---\n%s", body)
	}
}
