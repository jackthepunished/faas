package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
)

func TestEnvExportPlaintextOnlySelectedScope(t *testing.T) {
	e := setup(t, api.PlanHobby)
	app := createApp(t, e, "export-app")
	ctx := context.Background()
	if err := e.store.UpsertAppEnvInScope(ctx, e.acct.ID, app.ID, "default", "PUBLIC", "default-value"); err != nil {
		t.Fatal(err)
	}
	if err := e.store.UpsertAppEnvInScope(ctx, e.acct.ID, app.ID, "staging", "PUBLIC", "staging-value\nwith-lines"); err != nil {
		t.Fatal(err)
	}
	if err := e.store.UpsertAppSecret(ctx, e.acct.ID, app.ID, "SEALED", []byte("sealed-private-sentinel")); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"default", "staging", "empty"} {
		response := e.do(t, "POST", "/v1/apps/"+app.Slug+"/env-export?scope="+scope, api.ExportAppEnvRequest{AcknowledgeSensitiveValues: true}, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("export: %d %s", response.Code, response.Body.String())
		}
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("plaintext response may be cached")
		}
		if strings.Contains(response.Body.String(), "SEALED") || strings.Contains(response.Body.String(), "sealed-private-sentinel") {
			t.Fatal("sealed secret leaked")
		}
		var result api.AppEnvExportResponse
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Scope != scope || result.AppSlug != app.Slug || result.Values == nil {
			t.Fatal("wrong export context")
		}
		switch scope {
		case "default":
			if result.Values["PUBLIC"] != "default-value" {
				t.Fatal("wrong default value")
			}
		case "staging":
			if result.Values["PUBLIC"] != "staging-value\nwith-lines" {
				t.Fatal("wrong staging value")
			}
		case "empty":
			if len(result.Values) != 0 {
				t.Fatal("empty scope fell back to default")
			}
		}
	}
	metadata := e.do(t, "GET", "/v1/apps/"+app.Slug+"/env", nil, nil)
	if strings.Contains(metadata.Body.String(), "default-value") {
		t.Fatal("metadata GET leaked value")
	}
}

func TestEnvExportRejectsUnacknowledgedAndInvalidScope(t *testing.T) {
	e := setup(t, api.PlanHobby)
	app := createApp(t, e, "export-invalid")
	if response := e.do(t, "POST", "/v1/apps/"+app.Slug+"/env-export", api.ExportAppEnvRequest{}, nil); response.Code != 400 {
		t.Fatalf("missing acknowledgement: %d", response.Code)
	}
	for _, scope := range []string{"__all__", "../invalid", "x"} {
		response := e.do(t, "POST", "/v1/apps/"+app.Slug+"/env-export?scope="+scope, api.ExportAppEnvRequest{AcknowledgeSensitiveValues: true}, nil)
		if response.Code != 400 {
			t.Fatalf("invalid scope %s: %d", scope, response.Code)
		}
	}
	if response := e.do(t, "GET", "/v1/apps/"+app.Slug+"/env-export", nil, nil); response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET allowed: %d", response.Code)
	}
}

func TestEnvExportRequiresEnvWritePermission(t *testing.T) {
	e := setup(t, api.PlanHobby)
	app := createApp(t, e, "export-permissions")
	for _, scopes := range [][]string{{api.ScopeAppsRead}, {api.ScopeEnvRead}, {api.ScopeEnvWrite}} {
		plaintext, hash, err := api.GenerateAPIKey()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.store.CreateAPIKey(context.Background(), e.acct.ID, hash, "export-test", scopes); err != nil {
			t.Fatal(err)
		}
		e.key = plaintext
		response := e.do(t, "POST", "/v1/apps/"+app.Slug+"/env-export", api.ExportAppEnvRequest{AcknowledgeSensitiveValues: true}, nil)
		expected := http.StatusForbidden
		if scopes[0] == api.ScopeEnvWrite {
			expected = http.StatusOK
		}
		if response.Code != expected {
			t.Fatalf("scope %s: %d %s", scopes[0], response.Code, response.Body.String())
		}
	}
}

func TestEnvExportAccountBoundary(t *testing.T) {
	e := setup(t, api.PlanHobby)
	app := createApp(t, e, "export-owner")
	other, err := e.store.CreateAccount(context.Background(), "export-other@example.com", api.PlanHobby)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, hash, err := api.GenerateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.CreateAPIKey(context.Background(), other.ID, hash, "other", api.ScopesAdminOnly); err != nil {
		t.Fatal(err)
	}
	e.key = plaintext
	response := e.do(t, "POST", "/v1/apps/"+app.Slug+"/env-export", api.ExportAppEnvRequest{AcknowledgeSensitiveValues: true}, nil)
	if response.Code != http.StatusNotFound {
		t.Fatalf("cross-account export: %d %s", response.Code, response.Body.String())
	}
}

func TestEnvExportDoesNotPersistIdempotencyValues(t *testing.T) {
	e := setup(t, api.PlanHobby)
	app := createApp(t, e, "export-no-replay")
	ctx := context.Background()
	if err := e.store.UpsertAppEnv(ctx, e.acct.ID, app.ID, "PUBLIC", "first"); err != nil {
		t.Fatal(err)
	}
	path := "/v1/apps/" + app.Slug + "/env-export"
	request := api.ExportAppEnvRequest{AcknowledgeSensitiveValues: true}
	headers := map[string]string{"Idempotency-Key": "fixed-export-key"}
	if response := e.do(t, "POST", path, request, headers); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	if err := e.store.UpsertAppEnv(ctx, e.acct.ID, app.ID, "PUBLIC", "second"); err != nil {
		t.Fatal(err)
	}
	response := e.do(t, "POST", path, request, headers)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "second") || response.Header().Get("Idempotent-Replayed") == "true" {
		t.Fatal("export persisted/replayed a plaintext response")
	}
}
