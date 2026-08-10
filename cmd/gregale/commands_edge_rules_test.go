// Tests for the `gregale edge-rules` subcommand dispatcher (PR 2 of
// the Edge Rules rollout). Mirrors commands_crons_runs_test.go and
// commands_deployments_test.go: httptest.NewServer fake + t.Setenv
// + osStdout swap. Per-kind builders are exercised via the public
// cmdEdgeRulesCreate leaves; the JSON path uses jsonOutput=true +
// the writeJSON/writeNDJSON envelope.
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
)

// edgeRuleTestID is the 32-hex id every test uses. Matches the
// edgeRuleID regex in pkg/api/handlers_edge_rules.go.
const edgeRuleTestID = "0123456789abcdef0123456789abcdef"

// edgeRuleTestAppSlug is the slug every list/create test uses. Kept
// short so the human-mode table renders without truncation in
// asserts.
const edgeRuleTestAppSlug = "demo"

// edgeRuleTestAccountID is the account_id echoed by the fake server
// for every response. Used by the get-table assertion.
const edgeRuleTestAccountID = "acc_0123456789abcdef0123456789abcdef"

// resetJSONEnv clears jsonOutput between subtests. The run() helper
// sets it from --json; tests that exercise the human path must reset
// to false explicitly so prior subtests don't leak.
func resetJSONEnv(t *testing.T) {
	t.Helper()
	resetJSONOutput()
}

// sampleEdgeRuleResponse builds a fully-populated EdgeRuleResponse
// for the table-rendering tests. All fields are realistic; tests
// that need a missing field build their own.
func sampleEdgeRuleResponse(id string) api.EdgeRuleResponse {
	return api.EdgeRuleResponse{
		ID:           id,
		AccountID:    edgeRuleTestAccountID,
		AppID:        "app_0123456789abcdef0123456789abcdef",
		MatchHost:    "demo.apps.gregale.dev",
		MatchPath:    "/",
		MatchMethods: []string{"GET", "POST"},
		Priority:     100,
		Enabled:      true,
		Kind:         "rewrite",
		Action:       json.RawMessage(`{"from":"/api","to":"/v1"}`),
		CreatedAt:    time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC),
		UpdatedAt:    time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC),
	}
}

// --- list -------------------------------------------------------------------

func TestCmdEdgeRulesList_HappyPath_ThreeRows(t *testing.T) {
	resetJSONEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]api.EdgeRuleResponse{
			sampleEdgeRuleResponse(edgeRuleTestID),
			sampleEdgeRuleResponse("abcdef0123456789abcdef0123456789"),
			sampleEdgeRuleResponse("fedcba9876543210fedcba9876543210"),
		})
	}))
	defer srv.Close()

	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	var stdout bytes.Buffer
	oldOut := osStdout
	osStdout = &stdout
	defer func() { osStdout = oldOut }()

	if code := cmdEdgeRulesList([]string{}); code != 0 {
		t.Errorf("list = %d, want 0", code)
	}
	body := stdout.String()
	if !strings.Contains(body, edgeRuleTestID) {
		t.Errorf("body missing id; got:\n%s", body)
	}
	if !strings.Contains(body, "rewrite") {
		t.Errorf("body missing kind column; got:\n%s", body)
	}
}

func TestCmdEdgeRulesList_EmptySentinel(t *testing.T) {
	resetJSONEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	var stdout bytes.Buffer
	oldOut := osStdout
	osStdout = &stdout
	defer func() { osStdout = oldOut }()

	if code := cmdEdgeRulesList([]string{}); code != 0 {
		t.Errorf("list empty = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "(no edge rules)") {
		t.Errorf("missing empty-list sentinel; got: %q", stdout.String())
	}
}

func TestCmdEdgeRulesList_JSON_Envelope(t *testing.T) {
	resetJSONEnv(t)
	jsonOutput = true
	defer func() { jsonOutput = false }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]api.EdgeRuleResponse{
			sampleEdgeRuleResponse(edgeRuleTestID),
		})
	}))
	defer srv.Close()

	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	if code := cmdEdgeRulesList([]string{}); code != 0 {
		t.Errorf("list --json = %d, want 0", code)
	}
}

func TestCmdEdgeRulesList_BadKind_NoServerCall(t *testing.T) {
	resetJSONEnv(t)
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	defer srv.Close()

	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	if code := cmdEdgeRulesList([]string{"--kind", "bogus"}); code == 0 {
		t.Errorf("expected non-zero exit on bad --kind")
	}
	if called {
		t.Errorf("server was called despite invalid --kind")
	}
}

// --- create -----------------------------------------------------------------

func TestCmdEdgeRulesCreate_BadKind_NoServerCall(t *testing.T) {
	resetJSONEnv(t)
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	defer srv.Close()

	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	code := cmdEdgeRulesCreate([]string{
		"--app", edgeRuleTestAppSlug,
		"--kind", "bogus",
		"--match-host", "x.example.com",
	})
	if code == 0 {
		t.Errorf("expected non-zero exit on bad --kind")
	}
	if called {
		t.Errorf("server was called despite invalid --kind")
	}
}

func TestCmdEdgeRulesCreate_Route_HappyPath(t *testing.T) {
	resetJSONEnv(t)
	var gotBody api.CreateEdgeRuleRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(sampleEdgeRuleResponse(edgeRuleTestID))
	}))
	defer srv.Close()

	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	code := cmdEdgeRulesCreate([]string{
		"--app", edgeRuleTestAppSlug,
		"--kind", "route",
		"--match-host", "x.example.com",
		"--route-target-slug", "target",
	})
	if code != 0 {
		t.Errorf("create route = %d, want 0", code)
	}
	if gotBody.Kind != "route" {
		t.Errorf("body.kind = %q, want route", gotBody.Kind)
	}
	var action api.EdgeRuleRouteAction
	if err := json.Unmarshal(gotBody.Action, &action); err != nil {
		t.Fatalf("action unmarshal: %v", err)
	}
	if action.TargetAppSlug != "target" {
		t.Errorf("action.target_app_slug = %q, want target", action.TargetAppSlug)
	}
}

func TestCmdEdgeRulesCreate_HappyPaths_AllKinds(t *testing.T) {
	cases := []struct {
		kind string
		args []string
	}{
		{"rewrite", []string{"--rewrite-from", "/api", "--rewrite-to", "/v1"}},
		{"redirect", []string{"--redirect-to", "https://x.example.com", "--redirect-status", "308"}},
		{"headers", []string{"--headers-request-add", "X-Foo:bar"}},
		{"cors", []string{"--cors-allow-origin", "https://x.example.com", "--cors-allow-method", "GET"}},
		{"jwt", []string{"--jwt-issuer", "https://issuer.example.com", "--jwt-jwks-url", "https://issuer.example.com/.well-known/jwks.json", "--jwt-algorithm", "RS256"}},
		{"ip", []string{"--ip-allow", "10.0.0.0/8"}},
	}
	for _, c := range cases {
		t.Run(c.kind, func(t *testing.T) {
			resetJSONEnv(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body api.CreateEdgeRuleRequest
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body.Kind != c.kind {
					t.Errorf("body.kind = %q, want %q", body.Kind, c.kind)
				}
				_ = json.NewEncoder(w).Encode(sampleEdgeRuleResponse(edgeRuleTestID))
			}))
			defer srv.Close()

			t.Setenv("FAAS_API", srv.URL)
			t.Setenv("FAAS_TOKEN", "fp_live_x")

			args := []string{
				"--app", edgeRuleTestAppSlug,
				"--kind", c.kind,
				"--match-host", "x.example.com",
			}
			args = append(args, c.args...)
			if code := cmdEdgeRulesCreate(args); code != 0 {
				t.Errorf("create %s = %d, want 0", c.kind, code)
			}
		})
	}
}

func TestCmdEdgeRulesCreate_JSON_Envelope(t *testing.T) {
	resetJSONEnv(t)
	jsonOutput = true
	defer func() { jsonOutput = false }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(sampleEdgeRuleResponse(edgeRuleTestID))
	}))
	defer srv.Close()

	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	code := cmdEdgeRulesCreate([]string{
		"--app", edgeRuleTestAppSlug,
		"--kind", "route",
		"--match-host", "x.example.com",
		"--route-target-slug", "target",
	})
	if code != 0 {
		t.Errorf("create route --json = %d, want 0", code)
	}
}

func TestCmdEdgeRulesCreate_BadCIDR_NoServerCall(t *testing.T) {
	resetJSONEnv(t)
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	defer srv.Close()

	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	code := cmdEdgeRulesCreate([]string{
		"--app", edgeRuleTestAppSlug,
		"--kind", "ip",
		"--match-host", "x.example.com",
		"--ip-allow", "not-a-cidr",
	})
	if code == 0 {
		t.Errorf("expected non-zero exit on bad CIDR")
	}
	if called {
		t.Errorf("server was called despite invalid CIDR")
	}
}

func TestCmdEdgeRulesCreate_BadJWTAlgorithm_NoServerCall(t *testing.T) {
	resetJSONEnv(t)
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	defer srv.Close()

	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	code := cmdEdgeRulesCreate([]string{
		"--app", edgeRuleTestAppSlug,
		"--kind", "jwt",
		"--match-host", "x.example.com",
		"--jwt-issuer", "https://issuer.example.com",
		"--jwt-jwks-url", "https://issuer.example.com/.well-known/jwks.json",
		"--jwt-algorithm", "BOGUS256",
	})
	if code == 0 {
		t.Errorf("expected non-zero exit on bad JWT algorithm")
	}
	if called {
		t.Errorf("server was called despite invalid JWT algorithm")
	}
}

// --- get --------------------------------------------------------------------

func TestCmdEdgeRulesGet_HappyPath(t *testing.T) {
	resetJSONEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(sampleEdgeRuleResponse(edgeRuleTestID))
	}))
	defer srv.Close()

	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	var stdout bytes.Buffer
	oldOut := osStdout
	osStdout = &stdout
	defer func() { osStdout = oldOut }()

	if code := cmdEdgeRulesGet([]string{edgeRuleTestID}); code != 0 {
		t.Errorf("get = %d, want 0", code)
	}
	body := stdout.String()
	if !strings.Contains(body, edgeRuleTestID) {
		t.Errorf("body missing id; got:\n%s", body)
	}
	if !strings.Contains(body, "rewrite") {
		t.Errorf("body missing kind; got:\n%s", body)
	}
}

func TestCmdEdgeRulesGet_JSON_Envelope(t *testing.T) {
	resetJSONEnv(t)
	jsonOutput = true
	defer func() { jsonOutput = false }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(sampleEdgeRuleResponse(edgeRuleTestID))
	}))
	defer srv.Close()

	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	if code := cmdEdgeRulesGet([]string{edgeRuleTestID}); code != 0 {
		t.Errorf("get --json = %d, want 0", code)
	}
}

// --- update -----------------------------------------------------------------

func TestCmdEdgeRulesUpdate_EnableFlag_SendsTruePointer(t *testing.T) {
	resetJSONEnv(t)
	var gotBody api.UpdateEdgeRuleRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(sampleEdgeRuleResponse(edgeRuleTestID))
	}))
	defer srv.Close()

	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	if code := cmdEdgeRulesUpdate([]string{"--enable", edgeRuleTestID}); code != 0 {
		t.Errorf("update --enable = %d, want 0", code)
	}
	if gotBody.Enabled == nil {
		t.Fatalf("enabled = nil, want pointer to true")
	}
	if !*gotBody.Enabled {
		t.Errorf("enabled = false, want true")
	}
}

func TestCmdEdgeRulesUpdate_MutuallyExclusiveFlags(t *testing.T) {
	resetJSONEnv(t)
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	defer srv.Close()

	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	code := cmdEdgeRulesUpdate([]string{"--enable", "--disable", edgeRuleTestID})
	if code == 0 {
		t.Errorf("expected non-zero exit on --enable + --disable")
	}
	if called {
		t.Errorf("server was called despite conflicting flags")
	}
}

// --- delete -----------------------------------------------------------------

func TestCmdEdgeRulesRm_QuietBypass_HappyPath(t *testing.T) {
	resetJSONEnv(t)
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != http.MethodDelete {
			t.Errorf("method = %q, want DELETE", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	if code := cmdEdgeRulesRm([]string{"--quiet", edgeRuleTestID}); code != 0 {
		t.Errorf("rm --quiet = %d, want 0", code)
	}
	if !called {
		t.Errorf("server was not called")
	}
}

func TestCmdEdgeRulesRm_CancelledByRequireTyped_NoServerCall(t *testing.T) {
	resetJSONEnv(t)
	// requireTyped reads from osStdin. Swap it for a buffer that
	// returns the wrong phrase so the confirm aborts.
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	defer srv.Close()

	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	oldIn := osStdin
	osStdin = strings.NewReader("no, don't delete it\n")
	defer func() { osStdin = oldIn }()

	if code := cmdEdgeRulesRm([]string{edgeRuleTestID}); code == 0 {
		t.Errorf("expected non-zero exit on cancel")
	}
	if called {
		t.Errorf("server was called despite cancelled confirm")
	}
}

// --- helpers ----------------------------------------------------------------

func TestBuildEdgeRuleAction_Headers_ConflictDetected(t *testing.T) {
	_, err := buildEdgeRuleAction("headers", edgeRuleActionInputs{
		HeadersReqAdd: []string{"X-Foo:bar"},
		HeadersReqSet: []string{"X-Foo:baz"},
	})
	if err == nil {
		t.Errorf("expected error on duplicate header name across add/set")
	}
}

func TestBuildEdgeRuleAction_JWT_BadAlgorithm(t *testing.T) {
	_, err := buildEdgeRuleAction("jwt", edgeRuleActionInputs{
		JWTIssuer:     "https://issuer.example.com",
		JWTJWKS:       "https://issuer.example.com/.well-known/jwks.json",
		JWTAlgorithms: []string{"BOGUS256"},
	})
	if err == nil {
		t.Errorf("expected error on bad JWT algorithm")
	}
}

func TestBuildEdgeRuleAction_IP_BadCIDR(t *testing.T) {
	_, err := buildEdgeRuleAction("ip", edgeRuleActionInputs{
		IPAllow: []string{"not-a-cidr"},
	})
	if err == nil {
		t.Errorf("expected error on bad CIDR")
	}
}
