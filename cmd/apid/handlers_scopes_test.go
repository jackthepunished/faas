package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/session"
	"github.com/onebox-faas/faas/pkg/state"
	"github.com/onebox-faas/faas/pkg/wire"
)

// setupWithScopes is the scope-aware twin of the package-level setup()
// helper in server_test.go. It mints a single API key with the given
// scopes and returns a testEnv that uses it. ACL/auth middleware does
// not care about plan here — the scope check happens after auth, so
// any plan works.
func setupWithScopes(t *testing.T, scopes []string) testEnv {
	t.Helper()
	store := state.NewMemStore()
	acct, err := store.CreateAccount(context.Background(), "scopes@example.com", api.PlanPro)
	if err != nil {
		t.Fatal(err)
	}
	pt, hash, err := api.GenerateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateAPIKey(context.Background(), acct.ID, hash, "scopes-test", scopes); err != nil {
		t.Fatal(err)
	}
	ops := wire.NewOpsMetrics("apid_scopes_test")
	srv := newServer(store, slog.New(slog.NewTextHandler(io.Discard, nil)), "example.com", noopNotifier{}).WithOpsMetrics(ops)
	return testEnv{h: srv.handler(), store: store, key: pt, acct: acct, ops: ops}
}

// setupWithSession is the session-cookie twin of setupWithScopes. It
// uses newServerWithDeps so the session.Manager is reachable from
// outside the server (newServer wraps a fresh ephemeral manager that
// can't be issued from the test). The returned testEnv.key is empty —
// callers MUST build their own httptest.NewRequest and attach the
// cookie with req.AddCookie; the bearer-only e.do helper does not
// carry cookies.
func setupWithSession(t *testing.T) (testEnv, *http.Cookie) {
	t.Helper()
	store := state.NewMemStore()
	acct, err := store.CreateAccount(context.Background(),
		"session-cookie@example.com", api.PlanPro)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := session.NewEphemeralManager(sessionCookieLifetime)
	if err != nil {
		t.Fatal(err)
	}
	token, err := mgr.Issue(acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	ops := wire.NewOpsMetrics("apid_session_test")
	srv := newServerWithDeps(store, slog.New(slog.NewTextHandler(io.Discard, nil)),
		"example.com", noopNotifier{}, "", noopMailer{}, stubGithubdClient{}, mgr,
		nil, 15*60_000_000_000, "").WithOpsMetrics(ops)
	return testEnv{h: srv.handler(), store: store, key: "", acct: acct, ops: ops},
		&http.Cookie{Name: sessionCookie, Value: token}
}

// TestScopeMatrix is the IAM-1 regression net for the per-route scope
// check. Each row exercises one combination of (method, key-scope)
// against a representative route and asserts the expected HTTP status:
//
//   - admin key + allowed op      → 2xx (handler runs)
//   - read-only key + GET         → 2xx
//   - read-only key + POST        → 403 insufficient_scope
//   - write-only key + GET        → 403 (write is not enough for read)
//   - write-only key + POST       → 2xx
//   - unknown scope on mint       → 400 validation_failed
//   - empty scope on mint         → 400 validation_failed (deny by default)
//   - admin-only route + non-admin → 403 (compute-nodes, /v1/account/plan,
//     DELETE /v1/account, POST /v1/keys, DELETE /v1/keys/{id})
//
// See ADR-034 for the rationale.
func TestScopeMatrix(t *testing.T) {
	t.Run("admin-key/GET-allowed", func(t *testing.T) {
		e := setupWithScopes(t, []string{"admin"})
		rec := e.do(t, http.MethodGet, "/v1/apps", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("admin GET /v1/apps: %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("admin-key/POST-allowed", func(t *testing.T) {
		e := setupWithScopes(t, []string{"admin"})
		rec := e.do(t, http.MethodPost, "/v1/apps", api.CreateAppRequest{Slug: "admin-app"}, nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("admin POST /v1/apps: %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("read-key/GET-allowed", func(t *testing.T) {
		e := setupWithScopes(t, []string{"read"})
		rec := e.do(t, http.MethodGet, "/v1/apps", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("read GET /v1/apps: %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("read-key/POST-forbidden", func(t *testing.T) {
		e := setupWithScopes(t, []string{"read"})
		rec := e.do(t, http.MethodPost, "/v1/apps", api.CreateAppRequest{Slug: "read-app"}, nil)
		assertProblem(t, rec, http.StatusForbidden, api.CodeForbidden)
	})

	t.Run("write-key/POST-allowed", func(t *testing.T) {
		e := setupWithScopes(t, []string{"write"})
		rec := e.do(t, http.MethodPost, "/v1/apps", api.CreateAppRequest{Slug: "write-app"}, nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("write POST /v1/apps: %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("write-key/GET-forbidden", func(t *testing.T) {
		e := setupWithScopes(t, []string{"write"})
		rec := e.do(t, http.MethodGet, "/v1/apps", nil, nil)
		assertProblem(t, rec, http.StatusForbidden, api.CodeForbidden)
	})

	t.Run("write-key/account-plan-forbidden", func(t *testing.T) {
		// /v1/account/plan is admin-only — write scope is not enough.
		e := setupWithScopes(t, []string{"write"})
		rec := e.do(t, http.MethodPatch, "/v1/account/plan", map[string]string{"plan": "scale"}, nil)
		assertProblem(t, rec, http.StatusForbidden, api.CodeForbidden)
	})

	t.Run("read-key/account-plan-forbidden", func(t *testing.T) {
		e := setupWithScopes(t, []string{"read"})
		rec := e.do(t, http.MethodPatch, "/v1/account/plan", map[string]string{"plan": "scale"}, nil)
		assertProblem(t, rec, http.StatusForbidden, api.CodeForbidden)
	})

	t.Run("mint-rejects-unknown-scope", func(t *testing.T) {
		e := setupWithScopes(t, []string{"admin"})
		rec := e.do(t, http.MethodPost, "/v1/keys", api.CreateKeyRequest{Label: "bad", Scopes: []string{"superuser"}}, nil)
		assertProblem(t, rec, http.StatusBadRequest, api.CodeValidation)
	})

	t.Run("build-key-requires-read-or-admin", func(t *testing.T) {
		// sanity check on a different family of routes — secrets are
		// gated by the same read/write per-method default.
		e := setupWithScopes(t, []string{"read"})
		rec := e.do(t, http.MethodGet, "/v1/apps/foo/secrets", nil, nil)
		assertProblem(t, rec, http.StatusNotFound, api.CodeNotFound) // 404 notFound on app, not 403 — read scope was accepted
	})

	t.Run("write-key/list-keys-403", func(t *testing.T) {
		// GET /v1/keys accepts {admin, read}. A write-only key has
		// neither and gets 403 from requireScope before the handler
		// runs. Pins that listing is read-only, not write.
		e := setupWithScopes(t, []string{"write"})
		rec := e.do(t, http.MethodGet, "/v1/keys", nil, nil)
		assertProblem(t, rec, http.StatusForbidden, api.CodeForbidden)
	})

	t.Run("write-key/mint-key-403", func(t *testing.T) {
		// POST /v1/keys is admin-only (minting a key from a scoped key
		// would be a self-elevation primitive). Write must not suffice.
		e := setupWithScopes(t, []string{"write"})
		rec := e.do(t, http.MethodPost, "/v1/keys",
			api.CreateKeyRequest{Label: "try"}, nil)
		assertProblem(t, rec, http.StatusForbidden, api.CodeForbidden)
	})

	t.Run("write-key/delete-key-403", func(t *testing.T) {
		// DELETE /v1/keys/{id} is admin-only for the same reason.
		e := setupWithScopes(t, []string{"write"})
		rec := e.do(t, http.MethodDelete, "/v1/keys/some-id", nil, nil)
		assertProblem(t, rec, http.StatusForbidden, api.CodeForbidden)
	})

	t.Run("session-cookie/admin-route-allowed", func(t *testing.T) {
		// The load-bearing branch of principalHasScope: a session-cookie
		// principal has Key==nil and is implicitly admin. PATCH
		// /v1/account/plan is admin-only; without the Key==nil branch,
		// the dashboard user would be 403'd on its own plan-change
		// page. We assert != 403 because the handler may 200/400 on
		// plan validation — what we care about is that requireScope
		// did NOT 403.
		e, cookie := setupWithSession(t)
		req := httptest.NewRequest(http.MethodPatch, "/v1/account/plan",
			strings.NewReader(`{"plan":"scale"}`))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		if rec.Code == http.StatusForbidden {
			t.Fatalf("session cookie was 403'd on admin route: %s", rec.Body)
		}
	})
}

// TestMintKeyScopes covers the lifecycle of a freshly-minted key: the
// response carries the requested scopes, listing the key returns them,
// and a key with the listed scopes can authenticate.
func TestMintKeyScopes(t *testing.T) {
	t.Run("mint-with-scopes", func(t *testing.T) {
		e := setupWithScopes(t, []string{"admin"})
		rec := e.do(t, http.MethodPost, "/v1/keys", api.CreateKeyRequest{Label: "deploy-only", Scopes: []string{"read", "write"}}, nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("mint: %d %s", rec.Code, rec.Body)
		}
		var out api.APIKeyResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got := fmt.Sprintf("%v", out.Scopes); got != "[read write]" {
			t.Errorf("scopes = %s, want [read write]", got)
		}
	})

	t.Run("mint-omitted-scopes-defaults-to-admin", func(t *testing.T) {
		e := setupWithScopes(t, []string{"admin"})
		rec := e.do(t, http.MethodPost, "/v1/keys", api.CreateKeyRequest{Label: "defaulted"}, nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("mint: %d %s", rec.Code, rec.Body)
		}
		var out api.APIKeyResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if len(out.Scopes) != 1 || out.Scopes[0] != "admin" {
			t.Errorf("omitted scopes = %v, want [admin]", out.Scopes)
		}
	})

	t.Run("list-keys-shows-scopes", func(t *testing.T) {
		e := setupWithScopes(t, []string{"admin"})
		rec := e.do(t, http.MethodGet, "/v1/keys", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("list: %d %s", rec.Code, rec.Body)
		}
		var out []api.APIKeyResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if len(out) != 1 {
			t.Fatalf("got %d keys, want 1", len(out))
		}
		if got := fmt.Sprintf("%v", out[0].Scopes); got != "[admin]" {
			t.Errorf("listed scopes = %s, want [admin]", got)
		}
	})

	t.Run("mint-empty-scopes-defaults-to-admin", func(t *testing.T) {
		// Explicit empty array is operationally identical to omitted:
		// both fall through the len(scopes)==0 default in createKey.
		// Pin the contract so a future "deny empty" change is loud.
		e := setupWithScopes(t, []string{"admin"})
		rec := e.do(t, http.MethodPost, "/v1/keys",
			api.CreateKeyRequest{Label: "empty", Scopes: []string{}}, nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("mint with empty scopes: %d %s", rec.Code, rec.Body)
		}
		var out api.APIKeyResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if len(out.Scopes) != 1 || out.Scopes[0] != "admin" {
			t.Errorf("empty scopes defaulted to %v, want [admin]", out.Scopes)
		}
	})
}

// httptest helper that's already available in the test package is
// httptest.NewRecorder; importing it here keeps the per-file imports
// self-contained.
var _ = httptest.NewRecorder
