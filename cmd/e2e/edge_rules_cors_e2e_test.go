// edge_rules_cors_e2e_test.go — D18 per-kind e2e for `kind=cors`.
// Bitmask: APID | Gatewayd. See edge_rules_common_test.go for the
// kind=route-substitute precondition pattern.

package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
	"github.com/onebox-faas/faas/pkg/e2etest"
	"github.com/onebox-faas/faas/pkg/state"
)

func TestEdgeRulesCORS_E2E(t *testing.T) {
	pool := pgtest.Open(t)
	if pool == nil {
		return
	}
	if err := db.MigrateUp(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	h := e2etest.StartWithEnv(t, pool, e2etest.APID|e2etest.Gatewayd, nil)
	key := h.SeedAccount(context.Background(), api.PlanHobby)
	accountID := accountIDFromKey(t, context.Background(), pool, key)

	slug := "cors-test-app"
	createRec := doReqBytes(t, h, key, http.MethodPost, "/v1/apps",
		api.CreateAppRequest{Slug: slug})
	if len(createRec) == 0 {
		t.Fatalf("create app: empty response")
	}
	var app api.AppResponse
	if err := json.Unmarshal(createRec, &app); err != nil {
		t.Fatalf("decode app: %v body=%s", err, createRec)
	}

	synthHost := "edgectl-cors.apps.test.example"

	seedRouteSubstitute(t, context.Background(), pool,
		accountID, app.ID, synthHost, slug)

	// Rule under test: kind=cors, AllowOrigins=[https://app.test],
	// AllowMethods=[POST,GET], AllowCredentials=true. The Origin
	// echo + Allow-Methods header are the assertion surface.
	seedEdgeRuleDirect(t, context.Background(), pool,
		accountID, app.ID, synthHost,
		state.EdgeRuleKindCORSA,
		map[string]any{
			"kind": "cors",
			"cors": map[string]any{
				"allow_origins":     []string{"https://app.test"},
				"allow_methods":     []string{"POST", "GET"},
				"allow_credentials": true,
			},
		},
	)

	resetEdgeRuleCache(t, h)

	// Happy path: OPTIONS preflight. CORS short-circuits with 204
	// BEFORE redirect (handler.go:2296). Browser preflight must NOT
	// get a 3xx.
	header, _, status := doReqHeaders(t, h, synthHost, http.MethodOptions,
		"/", nil, map[string]string{
			"Origin":                         "https://app.test",
			"Access-Control-Request-Method":  "POST",
			"Access-Control-Request-Headers": "X-Requested-With",
		})
	if status != http.StatusNoContent {
		t.Errorf("kind=cors preflight: status=%d, want 204", status)
	}
	if got := header.Get("Access-Control-Allow-Origin"); got != "https://app.test" {
		t.Errorf("kind=cors preflight: ACAO=%q, want https://app.test", got)
	}
	if got := header.Get("Access-Control-Allow-Methods"); got != "" && !stringContains(got, "POST") {
		t.Errorf("kind=cors preflight: ACAM=%q, want to contain POST", got)
	}
	if got := header.Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("kind=cors preflight: ACAC=%q, want true", got)
	}

	// Negative path: a host with NO kind=cors rule but with the
	// kind=route substitute, hitting OPTIONS preflight without a
	// matching CORS rule falls through to Backend.Pick → 404 (no
	// routable target). This is distinct from a 204 with no CORS
	// headers — gateway's CORS hook returns false when no rule
	// matches.
	header, _, status = doReqHeaders(t, h, "edgectl-nocors.apps.test.example",
		http.MethodOptions, "/", nil, map[string]string{
			"Origin":                        "https://app.test",
			"Access-Control-Request-Method": "POST",
		})
	if status == http.StatusNoContent {
		t.Errorf("kind=cors negative: status=%d, want NOT 204 (no CORS rule → no short-circuit)", status)
	}
	if got := header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("kind=cors negative: ACAO=%q, want empty (no CORS rule)", got)
	}
}

func stringContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
