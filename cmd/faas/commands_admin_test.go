// Issue #279 PR A — `faas admin credit <account> <cents> --reason`
// CLI smoke tests. Pins the three contracts:
//
//  1. Argument validation: bad UUID, bad cents, missing reason each
//     print a usage line and exit 2 (the CLI convention for
//     operator-error inputs).
//  2. Happy path: a single POST /v1/admin/accounts/{id}/credits hits
//     the API with the explicit Idempotency-Key header and the
//     {cents, reason} JSON body.
//  3. Auth gate: no token → exit 2 (the documented errAuth path).
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/onebox-faas/faas/pkg/api"
)

func TestAdminCredit_BadUUIDExitsTwo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FAAS_TOKEN", "test")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("no network call expected on bad UUID")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("FAAS_API", srv.URL)

	code := cmdAdmin([]string{"credit", "not-a-uuid", "500", "--reason", "goodwill for outage"})
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestAdminCredit_BadCentsExitsTwo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FAAS_TOKEN", "test")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("no network call expected on bad cents")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("FAAS_API", srv.URL)

	code := cmdAdmin([]string{"credit", uuid.NewString(), "not-an-int", "--reason", "goodwill for outage"})
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestAdminCredit_MissingReasonExitsTwo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FAAS_TOKEN", "test")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("no network call expected on missing --reason")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("FAAS_API", srv.URL)

	// Flag-before-positional; --reason absent → the empty-string
	// validator fires after fs.Parse.
	code := cmdAdmin([]string{"credit", uuid.NewString(), "500"})
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestAdminCredit_NoTokenExitsTwo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FAAS_TOKEN", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("no network call expected without a token")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("FAAS_API", srv.URL)

	code := cmdAdmin([]string{"credit", uuid.NewString(), "500", "--reason", "goodwill for outage"})
	if code != 2 {
		t.Errorf("exit code = %d, want 2 (auth)", code)
	}
}

func TestAdminCredit_HappyPathHitsAPID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FAAS_TOKEN", "test")

	targetID := uuid.NewString()
	const wantReason = "goodwill for outage"

	var hits [3]string // [path, idem key, body]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/credits"):
			hits[0] = r.URL.Path
			hits[1] = r.Header.Get("Idempotency-Key")
			var body struct {
				Cents  int64  `json:"cents"`
				Reason string `json:"reason"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode body: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			hits[2] = body.Reason
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(api.AccountCreditResponse{
				ID:             uuid.NewString(),
				AccountID:      targetID,
				CentsRemaining: body.Cents,
				Reason:         body.Reason,
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("FAAS_API", srv.URL)

	// Go's flag package stops parsing flags once a positional arg is
	// seen; the documented convention is flags-before-positional.
	// cmdAccountExport's `--no-secrets` test pins the same pattern.
	code := cmdAdmin([]string{"credit", "--reason", wantReason, targetID, "500"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.HasSuffix(hits[0], "/"+targetID+"/credits") {
		t.Errorf("path = %q, want suffix /%s/credits", hits[0], targetID)
	}
	if !strings.HasPrefix(hits[1], "cli-admin-credit-") {
		t.Errorf("Idempotency-Key = %q, want cli-admin-credit-*", hits[1])
	}
	if hits[2] != wantReason {
		t.Errorf("body reason = %q, want %q", hits[2], wantReason)
	}
}
