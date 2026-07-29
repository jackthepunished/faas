package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/auth"
	"github.com/onebox-faas/faas/pkg/state"
)

// TestGoogleAuthRedirect drives GET /v1/auth/google with the OAuth
// config wired as Configured (issue #419 / ADR-046: the handler now
// reads s.oauthConfig, not os.Getenv). Expects a 302 to the Google
// consent screen, plus a faas_google_state CSRF cookie scoped to
// /v1/auth/google/callback.
func TestGoogleAuthRedirect(t *testing.T) {
	cfg := auth.SignInConfig{
		Google: auth.SignInProvider{
			Status:       auth.SignInProviderConfigured,
			ClientID:     "test_google_client_id",
			ClientSecret: "test_google_client_secret",
		},
	}
	store := state.NewMemStore()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := newServer(store, log, "example.com", noopNotifier{}).WithOAuthConfig(cfg)

	req := httptest.NewRequest("GET", "/v1/auth/google", nil)
	w := httptest.NewRecorder()

	srv.handler().ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected status 302 Found, got %d", resp.StatusCode)
	}

	loc := resp.Header.Get("Location")
	if !strings.Contains(loc, "accounts.google.com/o/oauth2/v2/auth") {
		t.Errorf("expected Location header pointing to Google OAuth, got %s", loc)
	}

	cookies := resp.Cookies()
	var foundStateCookie bool
	for _, c := range cookies {
		if c.Name == googleAuthStateCookie {
			foundStateCookie = true
			if c.Value == "" {
				t.Errorf("expected non-empty state cookie value")
			}
		}
	}

	if !foundStateCookie {
		t.Errorf("expected faas_google_state CSRF cookie to be set")
	}
}

// TestGoogleAuthRedirect_BothEnvUnset asserts the consent-redirect
// path is fail-closed when both GOOGLE_CLIENT_ID and
// GOOGLE_CLIENT_SECRET are unset on this host (issue #419 /
// ADR-046). 503 oauth_provider_unavailable so operators see the
// misconfiguration in logs immediately. Distinct from the legacy
// 500 google_oauth_misconfigured (defence-in-depth). The name
// reflects the post-#419 shape: "both unset" is what reaches the
// runtime — half-set (one env var set) refuses to boot, so the
// runtime never sees it.
func TestGoogleAuthRedirect_BothEnvUnset(t *testing.T) {
	store := state.NewMemStore()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// oauthConfig zero-value = both providers Disabled (the issue
	// #419 shape, before cmd/apid/main.go::runWithDeps runs).
	srv := newServer(store, log, "example.com", noopNotifier{})

	req := httptest.NewRequest("GET", "/v1/auth/google", nil)
	w := httptest.NewRecorder()

	srv.handler().ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}
	var p map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if p["code"] != "oauth_provider_unavailable" {
		t.Errorf("code = %v, want oauth_provider_unavailable", p["code"])
	}
}

func TestGoogleAuthCallbackCSRFMismatch(t *testing.T) {
	store := state.NewMemStore()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := newServer(store, log, "example.com", noopNotifier{})

	req := httptest.NewRequest("GET", "/v1/auth/google/callback?state=invalid_state&code=test_code", nil)
	w := httptest.NewRecorder()

	srv.handler().ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected status 400 Bad Request for CSRF state mismatch, got %d", resp.StatusCode)
	}
}
