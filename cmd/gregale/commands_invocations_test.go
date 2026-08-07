// commands_invocations_test.go — issue #315 / tier-2 DX.
//
// Tests for `gregale invocation <id> [--json] [--replay]`. Mirrors
// commands_metrics_test.go shape: httptest fake-apid that records
// the request, t.Setenv for FAAS_API / FAAS_TOKEN, swapIO for the
// human-mode labelled-block assertions, and an inline fixture for
// the api.Invocation payload.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
)

// invocationFixture returns a deterministic invocation row for the
// happy-path tests. Source/method/path mirror what an async-invoke
// replay would carry; the customer-facing labels (App, Method,
// Path, etc.) are stable strings so the assertion blocks below can
// substring-match without coupling to the json marshaler.
func invocationFixture() api.Invocation {
	completed := time.Date(2026, 8, 7, 12, 34, 57, 0, time.UTC)
	created := completed.Add(-time.Second)
	return api.Invocation{
		ID:          "01abc-invocation",
		AppID:       "my-app",
		AccountID:   "acct-1",
		Source:      "async_invoke",
		State:       "completed",
		Method:      "POST",
		Path:        "/api/charge",
		Payload:     json.RawMessage(`{"amount":42}`),
		Result:      json.RawMessage(`{"ok":true,"txn_id":"t-1"}`),
		Attempts:    1,
		CreatedAt:   created,
		CompletedAt: &completed,
	}
}

// --- happy paths ------------------------------------------------------------

// TestCmdInvocation_HappyPath pins the labelled-block render for a
// successful read. Verifies path, query (no flags → no query), and
// every human-mode label against the fixture.
func TestCmdInvocation_HappyPath(t *testing.T) {
	inv := invocationFixture()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/invocations/"+inv.ID {
			t.Errorf("path = %q, want /v1/invocations/%s", r.URL.Path, inv.ID)
		}
		_ = json.NewEncoder(w).Encode(inv)
	}))
	defer srv.Close()

	stdout, _, restore := swapIO(t)
	defer restore()
	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	if code := cmdInvocation([]string{inv.ID}); code != 0 {
		t.Fatalf("invocation = %d, want 0", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"Invocation:", inv.ID,
		"App:", "my-app",
		"Source:", "async_invoke",
		"State:", "completed",
		"Method:", "POST",
		"Path:", "/api/charge",
		"Attempts:", "1",
		"Result:", `{"ok":true,"txn_id":"t-1"}`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\nfull: %s", want, out)
		}
	}
}

// TestCmdInvocation_NoPositional ensures the usage line renders when
// the customer forgets the id.
func TestCmdInvocation_NoPositional(t *testing.T) {
	_, stderr, restore := swapIO(t)
	defer restore()
	t.Setenv("FAAS_API", "http://example.invalid")
	t.Setenv("FAAS_TOKEN", "fp_live_x")
	if code := cmdInvocation(nil); code != 1 {
		t.Errorf("invocation bare = %d, want 1 (usage)", code)
	}
	if !strings.Contains(stderr(), "usage: gregale invocation") {
		t.Errorf("stderr missing usage line\nfull: %s", stderr())
	}
}

// TestCmdInvocation_Unauthenticated: no token + empty HOME →
// authedClient fails. Mirrors TestCmdMetrics_Unauthenticated.
func TestCmdInvocation_Unauthenticated(t *testing.T) {
	t.Setenv("FAAS_TOKEN", "")
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	if code := cmdInvocation([]string{"abc"}); code == 0 {
		t.Error("invocation without token must fail")
	}
}

// --- JSON output ------------------------------------------------------------

// TestCmdInvocation_JSON_SingleRecord: --json emits one indented
// JSON object (writeJSON). Shape matches api.Invocation verbatim —
// no client-side reshaping.
func TestCmdInvocation_JSON_SingleRecord(t *testing.T) {
	inv := invocationFixture()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(inv)
	}))
	defer srv.Close()

	stdout, _, restore := swapIO(t)
	defer restore()
	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	resetJSONOutput()
	t.Cleanup(resetJSONOutput)
	jsonOutput = true

	if code := cmdInvocation([]string{inv.ID}); code != 0 {
		t.Fatalf("invocation --json = %d, want 0", code)
	}
	var got api.Invocation
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("JSON unmarshal: %v\nfull: %s", err, stdout.String())
	}
	if got.ID != inv.ID {
		t.Errorf("id = %q, want %q", got.ID, inv.ID)
	}
	if got.State != "completed" {
		t.Errorf("state = %q, want completed", got.State)
	}
}

// --- replay happy path ------------------------------------------------------

// TestCmdInvocation_ReplayHappyPath: --replay issues a POST against
// /v1/invocations/{id}/replay AND a prior GET against the read
// endpoint (cmdInvocation always reads first, then replays). The
// fake-apid returns the replay's AsyncInvokeResponse and the test
// asserts the labelled-block contains the new id + status URL.
func TestCmdInvocation_ReplayHappyPath(t *testing.T) {
	inv := invocationFixture()
	replayID := "01xyz-replayed"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/invocations/"+inv.ID:
			_ = json.NewEncoder(w).Encode(inv)
		case r.Method == "POST" && r.URL.Path == "/v1/invocations/"+inv.ID+"/replay":
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(api.AsyncInvokeResponse{
				ID:        replayID,
				StatusURL: "/v1/invocations/" + replayID,
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	stdout, _, restore := swapIO(t)
	defer restore()
	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	if code := cmdInvocation([]string{"--replay", inv.ID}); code != 0 {
		t.Fatalf("invocation --replay = %d, want 0", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"Invocation:", inv.ID,
		"App:", "my-app",
		"Replay id:", replayID,
		"Replay status:", "/v1/invocations/" + replayID,
		"Poll with:", "gregale invocation", replayID,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\nfull: %s", want, out)
		}
	}
}

// TestCmdInvocation_ReplayJSONEnvelope: --json + --replay emit a
// single {"original": ..., "replay": ...} object. Scripts depend on
// the stable shape (UX §3.2 "agents depend on it").
//
// Test pattern mirrors TestCmdMetrics_JSON_SingleRecord: set
// jsonOutput=true directly (rather than passing --json through the
// args) because cmdInvocation's flag set only knows --replay. The
// top-level run() dispatcher strips --json before reaching
// cmdInvocation (main.go:101); this test exercises the printer
// branch in isolation.
func TestCmdInvocation_ReplayJSONEnvelope(t *testing.T) {
	inv := invocationFixture()
	replayID := "01xyz-replayed"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET":
			_ = json.NewEncoder(w).Encode(inv)
		case r.Method == "POST":
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(api.AsyncInvokeResponse{
				ID:        replayID,
				StatusURL: "/v1/invocations/" + replayID,
			})
		}
	}))
	defer srv.Close()

	stdout, _, restore := swapIO(t)
	defer restore()
	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	resetJSONOutput()
	t.Cleanup(resetJSONOutput)
	jsonOutput = true

	if code := cmdInvocation([]string{"--replay", inv.ID}); code != 0 {
		t.Fatalf("invocation --replay (json mode) = %d, want 0", code)
	}
	var env struct {
		Original api.Invocation          `json:"original"`
		Replay   api.AsyncInvokeResponse `json:"replay"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("JSON unmarshal: %v\nfull: %s", err, stdout.String())
	}
	if env.Original.ID != inv.ID {
		t.Errorf("original.id = %q, want %q", env.Original.ID, inv.ID)
	}
	if env.Replay.ID != replayID {
		t.Errorf("replay.id = %q, want %q", env.Replay.ID, replayID)
	}
}

// TestCmdInvocation_Replay_NotReplayable: server returns 409
// ErrInvocationNotReplayable. The CLI surfaces it via renderAPIError
// and exits with the status-mapped exit code (4 for 409).
func TestCmdInvocation_Replay_NotReplayable(t *testing.T) {
	inv := invocationFixture()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET":
			_ = json.NewEncoder(w).Encode(inv)
		case r.Method == "POST":
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(api.Problem{
				Code:   api.CodeInvocationNotReplayable,
				Status: http.StatusConflict,
				Title:  "Invocation is not in a replayable state",
				Detail: "only invocations in state 'failed' or 'dead_letter' can be replayed; current state is \"completed\".",
			})
		}
	}))
	defer srv.Close()

	_, stderr, restore := swapIO(t)
	defer restore()
	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	code := cmdInvocation([]string{"--replay", inv.ID})
	if code == 0 {
		t.Fatalf("replay on completed invocation must fail; got exit 0")
	}
	// exitCodeForStatus(409) — confirm via stderr hint.
	if !strings.Contains(stderr(), "not in a replayable state") {
		t.Errorf("stderr missing problem detail\nfull: %s", stderr())
	}
}

// TestCmdInvocation_Replay_NotFound: cross-tenant or unknown id —
// 404 ErrInvocationNotFound. Mirrors the IDOR-safe contract.
func TestCmdInvocation_Replay_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(api.Problem{
			Code:   api.CodeInvocationNotFound,
			Status: http.StatusNotFound,
			Title:  "Invocation not found",
			Detail: "no invocation with id \"x\" on this account.",
		})
	}))
	defer srv.Close()

	_, stderr, restore := swapIO(t)
	defer restore()
	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	code := cmdInvocation([]string{"--replay", "x"})
	if code == 0 {
		t.Fatalf("replay on missing invocation must fail; got exit 0")
	}
	if !strings.Contains(stderr(), "Invocation not found") {
		t.Errorf("stderr missing 404 title\nfull: %s", stderr())
	}
}

// TestCmdInvocation_Read_NotFound: bare read with unknown id → 404.
// Sanity test that the read path also routes through renderAPIError.
func TestCmdInvocation_Read_NotFound(t *testing.T) {
	hits := int32(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(api.Problem{
			Code:   api.CodeInvocationNotFound,
			Status: http.StatusNotFound,
			Title:  "Invocation not found",
		})
	}))
	defer srv.Close()

	_, stderr, restore := swapIO(t)
	defer restore()
	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	code := cmdInvocation([]string{"missing-id"})
	if code == 0 {
		t.Fatalf("read on missing invocation must fail; got exit 0")
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Errorf("server hits = %d, want 1 (the read path)", hits)
	}
	if !strings.Contains(stderr(), "Invocation not found") {
		t.Errorf("stderr missing 404 title\nfull: %s", stderr())
	}
}

// --- dispatch ---------------------------------------------------------------

// TestRun_DispatchInvocation: main.go's case "invocation" routes to
// cmdInvocation. Mirrors TestRun_DispatchMetrics.
func TestRun_DispatchInvocation(t *testing.T) {
	inv := invocationFixture()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(inv)
	}))
	defer srv.Close()

	stdout, _, restore := swapIO(t)
	defer restore()
	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	if code := run([]string{"invocation", inv.ID}); code != 0 {
		t.Fatalf("run invocation = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "Invocation:") {
		t.Errorf("dispatch did not reach cmdInvocation; got %q", stdout.String())
	}
}
