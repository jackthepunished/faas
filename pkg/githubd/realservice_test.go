// RealService tests (slice 8, ADR-012). Covers the full
// githubdgrpc.Service surface: bindings, install-state, write-check.
package githubd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/githubdgrpc"
)

// newTestRealService builds a RealService with the in-memory
// BindingsStore and a pre-seeded install state. PR-B requires
// both: BindAppRepo needs the install id (it stamps the durable
// row), and the unit tests cover the success path end-to-end.
func newTestRealService(t *testing.T, accountID string) *RealService {
	t.Helper()
	store := newMemBindingsStore()
	svc := NewRealService(nil, nil, nil, store)
	if accountID != "" {
		// Seed the install state so BindAppRepo can resolve installID.
		if _, err := svc.ExchangeOAuthCode(accountID, "1", "main"); err != nil {
			t.Fatalf("seed install state: %v", err)
		}
	}
	return svc
}

func TestRealService_BindAndLookup(t *testing.T) {
	svc := newTestRealService(t, "acct-1")
	id, err := svc.BindAppRepo("app-1", "acct-1", "octo/api", "main")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty binding id")
	}
	b, err := svc.GetAppBinding("app-1", "acct-1")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if b.RepoFullName != "octo/api" {
		t.Errorf("repo = %q, want octo/api", b.RepoFullName)
	}
	if b.ProductionBranch != "main" {
		t.Errorf("branch = %q, want main", b.ProductionBranch)
	}
	if b.BindingID != id {
		t.Errorf("binding id mismatch: got %q, want %q", b.BindingID, id)
	}
}

func TestRealService_BindDefaultsToMain(t *testing.T) {
	svc := newTestRealService(t, "acct-1")
	if _, err := svc.BindAppRepo("app-2", "acct-1", "octo/api", ""); err != nil {
		t.Fatal(err)
	}
	b, _ := svc.GetAppBinding("app-2", "acct-1")
	if b.ProductionBranch != "main" {
		t.Errorf("default branch = %q, want main", b.ProductionBranch)
	}
}

func TestRealService_UnbindRemovesBinding(t *testing.T) {
	svc := newTestRealService(t, "acct-1")
	if _, err := svc.BindAppRepo("app-3", "acct-1", "octo/api", "main"); err != nil {
		t.Fatal(err)
	}
	if err := svc.UnbindAppRepo("app-3", "acct-1"); err != nil {
		t.Fatal(err)
	}
	b, _ := svc.GetAppBinding("app-3", "acct-1")
	if b.BindingID != "" {
		t.Errorf("after unbind, binding = %+v, want empty", b)
	}
	// Idempotent: second unbind is a no-op.
	if err := svc.UnbindAppRepo("app-3", "acct-1"); err != nil {
		t.Errorf("second unbind: %v", err)
	}
}

func TestRealService_InstallStateDefaults(t *testing.T) {
	svc := newTestRealService(t, "")
	state, instID, branch, err := svc.GetInstallState("acct-none")
	if err != nil {
		t.Fatal(err)
	}
	if state != githubdgrpc.InstallStateUnspecified {
		t.Errorf("state = %v, want Unspecified", state)
	}
	if instID != "" || branch != "" {
		t.Errorf("got non-empty install id/branch: %q/%q", instID, branch)
	}
}

func TestRealService_ExchangeOAuthCodePersists(t *testing.T) {
	svc := newTestRealService(t, "")
	id, err := svc.ExchangeOAuthCode("acct-1", "12345", "main")
	if err != nil {
		t.Fatal(err)
	}
	if id != "12345" {
		t.Errorf("id = %q, want 12345", id)
	}
	state, instID, branch, _ := svc.GetInstallState("acct-1")
	if state != githubdgrpc.InstallStateInstalled {
		t.Errorf("state = %v, want Installed", state)
	}
	if instID != "12345" || branch != "main" {
		t.Errorf("got %q/%q, want 12345/main", instID, branch)
	}
}

func TestRealService_WriteCheckRequiresConfig(t *testing.T) {
	svc := newTestRealService(t, "")
	err := svc.WriteCheck("octo/api", "abc", githubdgrpc.CheckPhaseQueued, "", "queued")
	if err == nil {
		t.Error("nil Checks writer should error")
	}
}

func TestRealService_ListInstallableReposRequiresAuth(t *testing.T) {
	svc := newTestRealService(t, "")
	_, err := svc.ListInstallableRepos("acct-1")
	if err == nil {
		t.Error("nil Auth should error")
	}
}

func TestRealService_ExchangeOAuthRejectsEmpty(t *testing.T) {
	svc := newTestRealService(t, "")
	if _, err := svc.ExchangeOAuthCode("", "1", "main"); err == nil {
		t.Error("empty accountID should error")
	}
	if _, err := svc.ExchangeOAuthCode("acct", "", "main"); err == nil {
		t.Error("empty installationID should error")
	}
}

func TestRealService_CreateDeploymentFromPushIsHTTPPath(t *testing.T) {
	svc := newTestRealService(t, "")
	_, _, err := svc.CreateDeploymentFromPush("octo/api", "refs/heads/main", "abc", "alice")
	if err == nil {
		t.Error("gRPC CreateDeploymentFromPush should error (webhook path is HTTP)")
	}
}

// _ pins context import so a future refactor that drops the only
// user doesn't drop the import.
var _ = context.Background

// TestRealService_VerifyInstallation_RequiresAuth asserts the
// §11 fail-closed behavior: a RealService built without OAuth
// credentials must refuse VerifyInstallation rather than silently
// returning verified=false (which the dashboard would treat as a
// "forged" callback and could confuse with a transient GitHub
// outage).
func TestRealService_VerifyInstallation_RequiresAuth(t *testing.T) {
	svc := newTestRealService(t, "")
	verified, _, _, err := svc.VerifyInstallation(1, "")
	if err == nil {
		t.Fatal("expected error when Auth is nil, got nil")
	}
	if verified {
		t.Errorf("verified = true, want false when Auth is nil")
	}
}

// TestRealService_VerifyInstallation_ForgedIsNotAnError asserts the
// reviewed contract: a forged installation_id returns
// (false, "", "", nil) — verified=false with err=nil — so the
// dashboard renders the "forged callback" banner rather than a 5xx
// page. A non-nil err is reserved for transport failures
// (api.github.com unreachable, App JWT rejected).
//
// We exercise this with an httptest.Server that returns 404 for
// every /app/installations/{id} request, mirroring GitHub's
// response to an unknown install_id.
func TestRealService_VerifyInstallation_ForgedIsNotAnError(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/app/installations/") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer fake.Close()

	auth := &AppAuth{AppID: "1", PrivateKey: newTestKey(t), HTTPClient: &singleHostClient{base: fake.Client(), api: fake.URL}}
	svc := NewRealService(auth, nil, nil, newMemBindingsStore())
	verified, _, branch, err := svc.VerifyInstallation(9999999, "")
	if err != nil {
		t.Fatalf("err = %v, want nil for forged install_id", err)
	}
	if verified {
		t.Errorf("verified = true, want false for forged install_id")
	}
	if branch != "" {
		t.Errorf("branch = %q, want empty for forged install_id", branch)
	}
}

// TestRealService_VerifyInstallation_TransportErrorIsErr asserts the
// inverse: a 5xx from api.github.com (anything not 200/404) comes
// back as a non-nil err so the dashboard can render a "couldn't
// reach GitHub" banner instead of a "forged callback" banner.
func TestRealService_VerifyInstallation_TransportErrorIsErr(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"boom"}`))
	}))
	defer fake.Close()

	auth := &AppAuth{AppID: "1", PrivateKey: newTestKey(t), HTTPClient: &singleHostClient{base: fake.Client(), api: fake.URL}}
	svc := NewRealService(auth, nil, nil, newMemBindingsStore())
	verified, _, _, err := svc.VerifyInstallation(1, "")
	if err == nil {
		t.Fatal("err = nil, want non-nil for 5xx response")
	}
	if verified {
		t.Errorf("verified = true, want false when err is non-nil")
	}
}

// TestRealService_VerifyInstallation_AccountLoginMismatchForged asserts
// the §11 ownership check (PR-B): a real install whose account.login
// does NOT match expectedLogin returns verified=false, err=nil —
// indistinguishable from a 404 to a forged caller. The dashboard
// distinguishes them by the AccountLogin field the apid-side
// comparison path consumes (it gets the install payload, not just
// the bool).
func TestRealService_VerifyInstallation_AccountLoginMismatchForged(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/app/installations/") {
			// The install is REAL — its account is "alice".
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":42,"account":{"login":"alice"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer fake.Close()

	auth := &AppAuth{AppID: "1", PrivateKey: newTestKey(t), HTTPClient: &singleHostClient{base: fake.Client(), api: fake.URL}}
	svc := NewRealService(auth, nil, nil, newMemBindingsStore())
	verified, _, _, err := svc.VerifyInstallation(42, "bob")
	if err != nil {
		t.Fatalf("err = %v, want nil for §11 mismatch (caller should treat as forged)", err)
	}
	if verified {
		t.Errorf("verified = true, want false for login mismatch")
	}
}

// TestRealService_VerifyInstallation_AccountLoginMatchAccepted asserts
// the §11 ownership check happy path: real install with matching
// account.login returns verified=true with the install's account
// login surfaced (so the apid handler can log it for the audit).
func TestRealService_VerifyInstallation_AccountLoginMatchAccepted(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/app/installations/") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":42,"account":{"login":"alice"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer fake.Close()

	auth := &AppAuth{AppID: "1", PrivateKey: newTestKey(t), HTTPClient: &singleHostClient{base: fake.Client(), api: fake.URL}}
	svc := NewRealService(auth, nil, nil, newMemBindingsStore())
	verified, login, _, err := svc.VerifyInstallation(42, "alice")
	if err != nil {
		t.Fatalf("err = %v, want nil for matching login", err)
	}
	if !verified {
		t.Errorf("verified = false, want true for matching login")
	}
	if login != "alice" {
		t.Errorf("account login = %q, want alice", login)
	}
}
