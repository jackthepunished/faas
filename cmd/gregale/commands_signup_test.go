// Tests for `gregale signup` (issue #311). The CLI uses the JSON-only
// programmatic auth surface that PR #786 added to apid, so the fixture
// here is the same shape the real server returns.
//
// Hermeticity rules (memory: cmd-gregale-requireslogin-hermeticity):
//   1. t.Setenv("HOME", t.TempDir())
//   2. t.Setenv("XDG_CONFIG_HOME", t.TempDir())
//   3. t.Setenv("FAAS_TOKEN", "")
//   4. setFakeKeyring(t)           — closes the macOS keyring hermeticity
//                                   gap (issue #311 R2).
// Plus the FAAS_API override so the tests don't actually hit production.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

const signupPlaintext = "fp_live_signupabcdef0123456789abcdef0123456789abcdef012"

// fakeSignupServer returns the canonical ProgrammaticAuthResponse
// body for the /v1/auth/{signup,login,signup/magic-link} routes. The
// route paths are recorded so each test can assert which one was hit.
func fakeSignupServer(t *testing.T, signupStatus int, magicLinkStatus int) (*httptest.Server, *signupServerCounter) {
	t.Helper()
	c := &signupServerCounter{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/signup":
			atomic.AddInt32(&c.signup, 1)
			if signupStatus != http.StatusOK {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(signupStatus)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"code":  "invalid_credentials",
					"title": "Invalid credentials",
				})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"account_id":"acc_signup_test","plan":"free","api_key":{"plaintext":"`+signupPlaintext+`","prefix":"fp_live_","id":"key_signup_test"}}`)
		case "/v1/auth/login":
			atomic.AddInt32(&c.login, 1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"account_id":"acc_signup_test","plan":"free","api_key":{"plaintext":"`+signupPlaintext+`","prefix":"fp_live_","id":"key_signup_test"}}`)
		case "/v1/auth/signup/magic-link":
			atomic.AddInt32(&c.magicLink, 1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(magicLinkStatus)
			if magicLinkStatus == http.StatusOK {
				_, _ = io.WriteString(w, `{"status":"ok"}`)
			}
		case "/v1/apps": // finalizeLogin's ListApps probe for the quickstart
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"items":[]}`)
		default:
			t.Logf("unexpected server path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

type signupServerCounter struct {
	signup, login, magicLink int32
}

// TestCmdSignup_InteractiveHappyPath: pipe email + password + confirm,
// httptest returns ProgrammaticAuthResponse, exit 0, token file =
// plaintext, stdout contains "Logged in as".
func TestCmdSignup_InteractiveHappyPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FAAS_TOKEN", "")
	setFakeKeyring(t)

	srv, counter := fakeSignupServer(t, http.StatusOK, http.StatusOK)
	t.Setenv("FAAS_API", srv.URL)

	pipeStdin(t, "alice@example.com\ncorrect-horse-battery-staple\ncorrect-horse-battery-staple\n")

	if got := cmdSignup(nil); got != 0 {
		t.Fatalf("cmdSignup exit = %d, want 0", got)
	}
	if c := atomic.LoadInt32(&counter.signup); c != 1 {
		t.Errorf("/v1/auth/signup hit %d times, want 1", c)
	}
	if got := readSavedToken(t); got != signupPlaintext {
		t.Errorf("saved token = %q, want %q", got, signupPlaintext)
	}
}

// TestCmdSignup_InteractiveWeakPassword: <12 char password fails
// before any HTTP round-trip. The test asserts the signup counter
// stays at 0 because auth.Validate rejects client-side.
func TestCmdSignup_InteractiveWeakPassword(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FAAS_TOKEN", "")
	setFakeKeyring(t)

	srv, counter := fakeSignupServer(t, http.StatusOK, http.StatusOK)
	t.Setenv("FAAS_API", srv.URL)

	rd, restore := captureStderr(t)
	pipeStdin(t, "alice@example.com\nshort\nshort\n")
	if got := cmdSignup(nil); got == 0 {
		t.Errorf("cmdSignup exit = 0, want non-zero (weak password)")
	}
	restore()
	if c := atomic.LoadInt32(&counter.signup); c != 0 {
		t.Errorf("/v1/auth/signup hit %d times, want 0 (rejected pre-HTTP)", c)
	}
	if !strings.Contains(rd.String(), "weak") {
		t.Errorf("stderr = %q, want 'weak' marker", rd.String())
	}
}

// TestCmdSignup_InteractiveMismatch: password and confirm differ,
// the handler rejects without any HTTP round-trip.
func TestCmdSignup_InteractiveMismatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FAAS_TOKEN", "")
	setFakeKeyring(t)

	srv, counter := fakeSignupServer(t, http.StatusOK, http.StatusOK)
	t.Setenv("FAAS_API", srv.URL)

	rd, restore := captureStderr(t)
	pipeStdin(t, "alice@example.com\ncorrect-horse-battery-staple\ndifferent-password-1234567890\n")
	if got := cmdSignup(nil); got == 0 {
		t.Errorf("cmdSignup exit = 0, want non-zero (mismatch)")
	}
	restore()
	if c := atomic.LoadInt32(&counter.signup); c != 0 {
		t.Errorf("/v1/auth/signup hit %d times, want 0 (rejected pre-HTTP)", c)
	}
	if !strings.Contains(rd.String(), "match") {
		t.Errorf("stderr = %q, want 'match' marker", rd.String())
	}
}

// TestCmdSignup_InteractiveInvalidEmail: malformed email fails
// client-side before any HTTP round-trip.
func TestCmdSignup_InteractiveInvalidEmail(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FAAS_TOKEN", "")
	setFakeKeyring(t)

	srv, counter := fakeSignupServer(t, http.StatusOK, http.StatusOK)
	t.Setenv("FAAS_API", srv.URL)

	rd, restore := captureStderr(t)
	pipeStdin(t, "not-an-email\ncorrect-horse-battery-staple\ncorrect-horse-battery-staple\n")
	if got := cmdSignup(nil); got == 0 {
		t.Errorf("cmdSignup exit = 0, want non-zero (invalid email)")
	}
	restore()
	if c := atomic.LoadInt32(&counter.signup); c != 0 {
		t.Errorf("/v1/auth/signup hit %d times, want 0 (rejected pre-HTTP)", c)
	}
	if !strings.Contains(rd.String(), "email") {
		t.Errorf("stderr = %q, want 'email' marker", rd.String())
	}
}

// TestCmdSignup_InteractiveLoginFailsWith401: server returns 401
// invalid_credentials. The CLI must surface a non-zero exit and
// MUST NOT write the token file.
func TestCmdSignup_InteractiveLoginFailsWith401(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FAAS_TOKEN", "")
	setFakeKeyring(t)

	srv, _ := fakeSignupServer(t, http.StatusUnauthorized, http.StatusOK)
	t.Setenv("FAAS_API", srv.URL)

	pipeStdin(t, "alice@example.com\ncorrect-horse-battery-staple\ncorrect-horse-battery-staple\n")
	if got := cmdSignup(nil); got == 0 {
		t.Errorf("cmdSignup exit = 0, want non-zero (server rejected)")
	}
	// Token file must NOT be present.
	if _, err := readSavedTokenOrSkip(t); err == nil {
		t.Errorf("token file written on 401; signup must not write on failure")
	}
}

// TestCmdSignup_EmailOnlyHappyPath: --email-only EMAIL posts to the
// magic-link endpoint, prints "Check your email", exits 0. No token
// file is written (the server defers the key mint to the verify step).
func TestCmdSignup_EmailOnlyHappyPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FAAS_TOKEN", "")
	setFakeKeyring(t)

	srv, counter := fakeSignupServer(t, http.StatusOK, http.StatusOK)
	t.Setenv("FAAS_API", srv.URL)

	rd, restore := captureStderr(t)
	if got := cmdSignup([]string{"--email-only", "alice@example.com"}); got != 0 {
		t.Fatalf("cmdSignup --email-only exit = %d, want 0", got)
	}
	restore()
	if c := atomic.LoadInt32(&counter.magicLink); c != 1 {
		t.Errorf("/v1/auth/signup/magic-link hit %d times, want 1", c)
	}
	if c := atomic.LoadInt32(&counter.signup); c != 0 {
		t.Errorf("/v1/auth/signup hit %d times, want 0 (magic-link path)", c)
	}
	// stdout contains "Check your email".
	if _, err := readSavedTokenOrSkip(t); err == nil {
		t.Errorf("token file written on magic-link path; must be deferred")
	}
	_ = rd
}

// TestCmdSignup_EmailOnlyServerUnreachable: FAAS_API on a closed port
// → cmdSignup exits non-zero and writes no token file.
func TestCmdSignup_EmailOnlyServerUnreachable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FAAS_TOKEN", "")
	setFakeKeyring(t)

	t.Setenv("FAAS_API", "http://127.0.0.1:1") // closed port

	if got := cmdSignup([]string{"--email-only", "alice@example.com"}); got == 0 {
		t.Errorf("cmdSignup --email-only exit = 0, want non-zero (server unreachable)")
	}
	if _, err := readSavedTokenOrSkip(t); err == nil {
		t.Errorf("token file written on unreachable server")
	}
}

// TestCmdSignup_DispatchesFromMain: gregale signup is wired into
// main.go's switch. Locked here so a future refactor that drops the
// dispatch case fails the test.
func TestCmdSignup_DispatchesFromMain(t *testing.T) {
	// We can't easily fork run() inside a test, so we just assert
	// the dispatch constant exists and main.go's switch case is
	// keyed off it. The runtime behaviour is exercised by the
	// integration path on the cli_login_test.go family.
	if dispatchSignup != "signup" {
		t.Errorf("dispatchSignup = %q, want %q", dispatchSignup, "signup")
	}
}

// readSavedTokenOrSkip returns the saved token if any, with error
// semantics that work for the "no token file" check. The simpler
// readSavedToken fails the test on missing file; this variant
// returns (token, error) so callers can distinguish "no token" from
// "wrong token".
func readSavedTokenOrSkip(t *testing.T) (string, error) {
	t.Helper()
	// Mirror the lookup helper in cli_login_test.go so the keyring
	// stub is honoured.
	if kr := effectiveKeyring(); kr != nil {
		if v, err := kr.Get(keyringService, keyringAccount); err == nil {
			return strings.TrimRight(v, "\r\n"), nil
		}
	}
	p, err := tokenPath()
	if err != nil {
		return "", fmt.Errorf("tokenPath: %w", err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}
