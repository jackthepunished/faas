// Tests for the `gregale webhooks <list|add|update|rm|deliveries|retry>`
// subcommands. Mirrors commands_crons_update_test.go's httptest + t.Setenv
// shape — the dispatch placement lives in main_test.go.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
)

// webhookTestID is the 32-hex id every test uses. Matches the
// webhookIDPattern in commands_webhooks.go.
const webhookTestID = "0123456789abcdef0123456789abcdef"

func TestCmdWebhooks_Add_HappyPath(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody api.CreateAppWebhookRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(api.AppWebhookResponse{
			ID:                        webhookTestID,
			AppID:                     "app-1",
			AccountID:                 "acct-1",
			TargetURL:                 "https://example.com/hook",
			WebhookSecretSealedMasked: api.AppWebhookSecretMasked,
			EventFilter:               []string{"cron.fired"},
			RetryPolicy:               "default",
			Enabled:                   true,
		})
	}))
	defer srv.Close()

	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	if code := cmdWebhooksAdd([]string{
		"--app", "demo",
		"--target-url", "https://example.com/hook",
		"--secret", "shh",
		"--event", "cron.fired",
	}); code != 0 {
		t.Errorf("add = %d, want 0", code)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1/apps/demo/webhooks" {
		t.Errorf("path = %q, want /v1/apps/demo/webhooks", gotPath)
	}
	if gotBody.WebhookSecret != "shh" {
		t.Errorf("body.webhook_secret = %q, want %q", gotBody.WebhookSecret, "shh")
	}
	if gotBody.RetryPolicy != "default" {
		t.Errorf("body.retry_policy = %q, want default", gotBody.RetryPolicy)
	}
}

func TestCmdWebhooks_Add_BadRetryPolicyRejected(t *testing.T) {
	// A typo in --retry-policy surfaces locally before the round-trip
	// (mirrors cmdApp's --eviction-priority check, PR #647).
	t.Setenv("FAAS_API", "http://localhost")
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	if code := cmdWebhooksAdd([]string{
		"--app", "demo",
		"--target-url", "https://example.com/hook",
		"--retry-policy", "defaultt",
	}); code == 0 {
		t.Errorf("typo --retry-policy accepted; want non-zero exit")
	}
}

func TestCmdWebhooks_Rm_HappyPath(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	if code := cmdWebhooksRm([]string{"--app", "demo", webhookTestID}); code != 0 {
		t.Errorf("rm = %d, want 0", code)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	if gotPath != "/v1/apps/demo/webhooks/"+webhookTestID {
		t.Errorf("path = %q, want /v1/apps/demo/webhooks/<id>", gotPath)
	}
}

func TestCmdWebhooks_Retry_HappyPath(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(api.AppWebhookRetryDeliveryResponse{
			Delivery: api.AppWebhookDeliveryResponse{
				ID:            "deadbeef00000000deadbeef00000000",
				WebhookID:     webhookTestID,
				Event:         "cron.fired",
				Attempt:       0,
				Status:        "pending",
				NextAttemptAt: "2026-08-06T12:00:00Z",
			},
		})
	}))
	defer srv.Close()

	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	if code := cmdWebhookRetry([]string{
		"--app", "demo",
		webhookTestID, "deadbeef00000000deadbeef00000000",
	}); code != 0 {
		t.Errorf("retry = %d, want 0", code)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if !strings.HasSuffix(gotPath, "/deliveries/deadbeef00000000deadbeef00000000/retry") {
		t.Errorf("path = %q, want suffix /deliveries/<did>/retry", gotPath)
	}
}
