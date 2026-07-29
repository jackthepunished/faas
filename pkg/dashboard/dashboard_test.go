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
