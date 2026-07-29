// commands_billing_test.go — issue #253 CLI surface pin.
//
// Pins the four documented behaviours of `faas billing portal`:
//   1. --print prints the URL and skips the browser
//   2. --no-open (default branch) opens via browser.Default (recorder)
//   3. empty URL → "portal not configured" friendly error, exit 1
//   4. no auth → exit 2 (handled by `requireNoAuth`)
//   5. unknown subcommand → usage error, exit 1
//
// The dispatcher (cmdBilling) is pinned by the subcommand routing:
// `faas billing help` exits 0; `faas billing bogus` exits 1.

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/browser"
)

// billingPortalStub is the minimal apid stub the CLI talks to: it
// answers GET /v1/billing/portal with either a populated URL or the
// empty-URL "absent" sentinel, depending on the configure() return.
func billingPortalStub(t *testing.T, configure func() api.BillingPortalResponse) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/billing/portal", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(configure())
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestCmdBillingPortal_PrintFlag(t *testing.T) {
	apiURL := billingPortalStub(t, func() api.BillingPortalResponse {
		return api.BillingPortalResponse{URL: "https://billing.example.com/portal?account=acct_42"}
	})
	t.Setenv("FAAS_API", apiURL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	// Swap the browser opener so an accidental browser call fails
	// the test loudly. --print must NOT call browser.Open.
	rec := withRecorder(t)

	stdout, restore := captureStdout(t)
	defer restore()
	if code := cmdBillingPortal([]string{"--print"}); code != 0 {
		t.Fatalf("cmdBillingPortal --print = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "https://billing.example.com/portal?account=acct_42") {
		t.Errorf("stdout missing URL; got: %q", stdout.String())
	}
	if len(rec.urls) != 0 {
		t.Errorf("--print opened browser %d times; want 0", len(rec.urls))
	}
}

func TestCmdBillingPortal_NoOpenAlias(t *testing.T) {
	apiURL := billingPortalStub(t, func() api.BillingPortalResponse {
		return api.BillingPortalResponse{URL: "https://billing.example.com/portal?account=acct_42"}
	})
	t.Setenv("FAAS_API", apiURL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	rec := withRecorder(t)
	stdout, restore := captureStdout(t)
	defer restore()
	if code := cmdBillingPortal([]string{"--no-open"}); code != 0 {
		t.Fatalf("cmdBillingPortal --no-open = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "https://billing.example.com/portal?account=acct_42") {
		t.Errorf("stdout missing URL; got: %q", stdout.String())
	}
	if len(rec.urls) != 0 {
		t.Errorf("--no-open opened browser %d times; want 0", len(rec.urls))
	}
}

func TestCmdBillingPortal_OpensBrowser(t *testing.T) {
	apiURL := billingPortalStub(t, func() api.BillingPortalResponse {
		return api.BillingPortalResponse{URL: "https://billing.example.com/portal?account=acct_42"}
	})
	t.Setenv("FAAS_API", apiURL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	rec := withRecorder(t)
	if code := cmdBillingPortal(nil); code != 0 {
		t.Fatalf("cmdBillingPortal (default) = %d, want 0", code)
	}
	if len(rec.urls) != 1 {
		t.Fatalf("recorder saw %d launches, want 1", len(rec.urls))
	}
	if rec.urls[0] != "https://billing.example.com/portal?account=acct_42" {
		t.Errorf("opened URL = %q, want the substituted portal link", rec.urls[0])
	}
}

func TestCmdBillingPortal_BrowserOpenFailureExitsZero(t *testing.T) {
	// Mirrors cmdDashboard: a failed browser launch is not a CLI
	// failure — the customer's intent ("get the portal URL") is
	// still satisfied via the stderr fallback. Exit 0.
	apiURL := billingPortalStub(t, func() api.BillingPortalResponse {
		return api.BillingPortalResponse{URL: "https://billing.example.com/portal?account=acct_42"}
	})
	t.Setenv("FAAS_API", apiURL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	rec := withRecorder(t)
	rec.err = errBrowserStub
	stderr, restore := captureStderr(t)
	defer restore()
	if code := cmdBillingPortal(nil); code != 0 {
		t.Errorf("cmdBillingPortal on browser failure = %d, want 0", code)
	}
	if !strings.Contains(stderr.String(), "Could not open browser") {
		t.Errorf("stderr missing browser-failure notice; got: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "https://billing.example.com/portal?account=acct_42") {
		t.Errorf("stderr missing URL fallback; got: %q", stderr.String())
	}
}

// errBrowserStub is the canned browser error used by the failure
// tests. We define it locally so the test file does not depend on
// internal pkg/browser symbols.
var errBrowserStub = errStub("browser.Open: stub failure")

type errStub string

func (e errStub) Error() string { return string(e) }

func TestCmdBillingPortal_EmptyURLReturnsFriendlyError(t *testing.T) {
	apiURL := billingPortalStub(t, func() api.BillingPortalResponse {
		return api.BillingPortalResponse{URL: ""}
	})
	t.Setenv("FAAS_API", apiURL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	stderr, restore := captureStderr(t)
	defer restore()
	if code := cmdBillingPortal(nil); code != 1 {
		t.Errorf("cmdBillingPortal on empty URL = %d, want 1 (user error)", code)
	}
	if !strings.Contains(stderr.String(), "Billing portal is not configured") {
		t.Errorf("stderr missing friendly hint; got: %q", stderr.String())
	}
}

func TestCmdBillingPortal_RequiresLogin(t *testing.T) {
	requireNoAuth(t)
	if code := cmdBillingPortal(nil); code != 2 {
		t.Errorf("cmdBillingPortal no-auth = %d, want 2", code)
	}
}

func TestCmdBillingPortal_RejectsExtraArgs(t *testing.T) {
	t.Setenv("FAAS_API", "http://localhost")
	t.Setenv("FAAS_TOKEN", "fp_live_x")
	if code := cmdBillingPortal([]string{"--print", "junk"}); code != 1 {
		t.Errorf("cmdBillingPortal with extra args = %d, want 1", code)
	}
}

func TestCmdBilling_Dispatch(t *testing.T) {
	t.Setenv("FAAS_API", "http://localhost")
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	t.Run("bare usage error", func(t *testing.T) {
		stderr, restore := captureStderr(t)
		defer restore()
		if code := cmdBilling(nil); code != 1 {
			t.Errorf("cmdBilling (no sub) = %d, want 1", code)
		}
		if !strings.Contains(stderr.String(), "usage: faas billing") {
			t.Errorf("stderr missing usage; got: %q", stderr.String())
		}
	})
	t.Run("help exits zero", func(t *testing.T) {
		stdout, restore := captureStdout(t)
		defer restore()
		if code := cmdBilling([]string{"help"}); code != 0 {
			t.Errorf("cmdBilling help = %d, want 0", code)
		}
		if !strings.Contains(stdout.String(), "portal") {
			t.Errorf("help output missing 'portal' subcommand; got: %q", stdout.String())
		}
	})
	t.Run("unknown subcommand exits 1", func(t *testing.T) {
		stderr, restore := captureStderr(t)
		defer restore()
		if code := cmdBilling([]string{"reticulate-splines"}); code != 1 {
			t.Errorf("cmdBilling unknown sub = %d, want 1", code)
		}
		if !strings.Contains(stderr.String(), "unknown subcommand") {
			t.Errorf("stderr missing 'unknown subcommand'; got: %q", stderr.String())
		}
	})
}

// captureStdout / captureStderr are declared in cli_login_test.go
// (commands2_test.go has the matching helpers). The browser package
// is referenced to ensure we exercise the seam that cmdBillingPortal
// touches.
var _ = browser.Default
