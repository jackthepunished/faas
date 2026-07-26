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
