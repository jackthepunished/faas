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
	"unicode/utf8"

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

// TestOneLine: peer review (PR #733 finding F3) flagged that
// oneLine used s[:max-1] (byte slice) which can split a multibyte
// rune and emit invalid UTF-8 to the terminal. The fix truncates
// on rune boundaries — this test pins the contract for both
// short (no truncation), exact-cap (no truncation), and the
// over-cap path that previously emitted invalid UTF-8.
//
// CJK rebar: 宁 = 3 bytes (E5 AE 81), so a 119-byte input with
// enough 3-byte glyphs forces the cut inside a multibyte sequence
// under the old code. The new path keeps the rune count at or
// under max-1 and emits a valid UTF-8 "…" suffix.
func TestOneLine(t *testing.T) {
	t.Run("short ASCII passes through", func(t *testing.T) {
		got := oneLine("hello world")
		if got != "hello world" {
			t.Errorf("oneLine(short ASCII) = %q, want %q", got, "hello world")
		}
	})
	t.Run("exact-rune-cap not truncated", func(t *testing.T) {
		// 120 runes of ASCII sits exactly at the cap; no truncation.
		s := strings.Repeat("a", 120)
		got := oneLine(s)
		if got != s {
			t.Errorf("oneLine(120 'a's) was truncated; rune count = %d", utf8.RuneCountInString(got))
		}
	})
	t.Run("over-cap truncated on rune boundary", func(t *testing.T) {
		// 200 3-byte CJK runes = 600 bytes. Under the old byte-slice
		// logic, s[:119] would split in the middle of a glyph and
		// emit invalid UTF-8. The new path keeps 119 runes + "…".
		s := strings.Repeat("宁", 200)
		got := oneLine(s)
		if !utf8.ValidString(got) {
			t.Fatalf("oneLine(%d '宁') = %q is not valid UTF-8", len(s), got)
		}
		if utf8.RuneCountInString(got) > 120 {
			t.Errorf("oneLine rune count = %d, want ≤ 120", utf8.RuneCountInString(got))
		}
		if !strings.HasSuffix(got, "…") {
			t.Errorf("oneLine over-cap must end with the ellipsis sentinel; got %q", got)
		}
	})
	t.Run("newlines collapsed to spaces", func(t *testing.T) {
		got := oneLine("line1\nline2\r\nline3")
		if got != "line1 line2  line3" {
			t.Errorf("oneLine(newlines) = %q, want %q", got, "line1 line2  line3")
		}
	})
}
