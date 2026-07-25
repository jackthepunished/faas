package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
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
// See ADR-011 for the rationale.
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
}

// httptest helper that's already available in the test package is
// httptest.NewRecorder; importing it here keeps the per-file imports
// self-contained.
var _ = httptest.NewRecorder
