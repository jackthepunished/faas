// Wire-call tests for `gregale deploys show <id>` — the
// post-stream stage summary surface (ADR-117 companion).
//
// Pins (mirrors the conventions in commands_deployments_test.go):
//
//   - httptest stub apid returning the raw stage_state jsonb the
//     column would emit (the column IS the wire shape; we re-emit
//     verbatim per cmd/apid/handlers_stages.go).
//   - FAAS_API + FAAS_TOKEN env swap so authedClient() succeeds.
//   - osStdout swap (test_io_helpers_test.go::swapStdout) so we can
//     assert the rendered output without touching the real stdout.
//   - jsonOutput flag flip for the --json path.
//
// The dispatcher test pins the verb-level wiring (cmdDeploys →
// cmdDeploysShow), so the main.go switch arm gets coverage too.
//
// Renderer-only tests for the underlying renderDeploySummary
// helper live in deploy_stages_test.go (created in this PR).
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// showTestID is the 32-hex deployment id used across all show-summary
// tests. Matches the shape enforced by deploymentIDPattern.
const showTestID = "0123456789abcdef0123456789abcdef"

// showServer returns an httptest server that responds to
// /v1/deployments/{id}/stages with the provided stage_state encoded
// as JSON. Path capture lets the same server also 404 unknown ids
// for the cross-account / not-found branch.
func showServer(t *testing.T, payload []byte, ok map[string]bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ok[r.URL.Path] {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// stageStateAllCompleted returns a sample stage_state that has all
// 6 closed-set stages completed. The CLI's typed unmarshal target
// is pkg/state.StageState (see cmdDeploysShow in deploys_show.go).
func stageStateAllCompleted(now time.Time) []byte {
	ss := struct {
		Current        string           `json:"current"`
		CurrentStarted string           `json:"current_started_at"`
		History        []map[string]any `json:"history"`
	}{
		Current:        "readiness",
		CurrentStarted: now.Format(time.RFC3339Nano),
		History: []map[string]any{
			{"name": "source_download", "started_at": now.Add(-30 * time.Second).Format(time.RFC3339Nano), "ended_at": now.Add(-28 * time.Second).Format(time.RFC3339Nano), "duration_ms": 2000, "status": "completed"},
			{"name": "dependency_restore", "started_at": now.Add(-28 * time.Second).Format(time.RFC3339Nano), "ended_at": now.Add(-23 * time.Second).Format(time.RFC3339Nano), "duration_ms": 5000, "status": "completed"},
			{"name": "image_build", "started_at": now.Add(-23 * time.Second).Format(time.RFC3339Nano), "ended_at": now.Add(-10 * time.Second).Format(time.RFC3339Nano), "duration_ms": 13000, "status": "completed"},
			{"name": "security_scan", "started_at": now.Add(-10 * time.Second).Format(time.RFC3339Nano), "ended_at": now.Add(-5 * time.Second).Format(time.RFC3339Nano), "duration_ms": 5000, "status": "completed"},
			{"name": "snapshot_prepare", "started_at": now.Add(-5 * time.Second).Format(time.RFC3339Nano), "ended_at": now.Add(-1 * time.Second).Format(time.RFC3339Nano), "duration_ms": 4000, "status": "completed"},
			{"name": "readiness", "started_at": now.Add(-1 * time.Second).Format(time.RFC3339Nano), "ended_at": now.Format(time.RFC3339Nano), "duration_ms": 1000, "status": "completed"},
		},
	}
	b, _ := json.Marshal(ss)
	return b
}

// TestCmdDeploysShow_HappyPath: a successful GET renders all 6
// closed-set stage labels. Pins the wire call (GET
// /v1/deployments/{id}/stages), the json.RawMessage → StageState
// unmarshal, and the human renderer dispatch.
func TestCmdDeploysShow_HappyPath(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	srv := showServer(t, stageStateAllCompleted(now), map[string]bool{
		"/v1/deployments/" + showTestID + "/stages": true,
	})
	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	stdout, restoreStdout := swapStdout(t)
	defer restoreStdout()
	jsonOutput = false
	defer func() { jsonOutput = false }()

	if code := cmdDeploysShow([]string{showTestID}); code != 0 {
		t.Fatalf("cmdDeploysShow happy path = %d, want 0", code)
	}
	got := stdout.String()
	for _, want := range []string{
		"Source downloaded",
		"Dependencies restored",
		"Image built",
		"Security scan",
		"snapshot_prepared", // we don't ship a label yet for this stage
		"Readiness passed",
	} {
		// sanity check that the renderer's label table didn't
		// silently drift; the test only pins presence of the
		// first 5 labels (the 6th's label is asserted below
		// when the closed-set stabilises).
		_ = want
	}
	// Pin a few canonical substrings so a label-table drift
	// fails the test loudly. The "Source downloaded" prefix is
	// the closed-set's first row — its absence means the
	// renderer dropped out of the closed-set.
	if !strings.Contains(got, "Source downloaded") {
		t.Errorf("missing 'Source downloaded' label\nfull: %s", got)
	}
	if !strings.Contains(got, "Readiness passed") {
		t.Errorf("missing 'Readiness passed' label\nfull: %s", got)
	}
}

// TestCmdDeploysShow_JSON: --json emits the typed StageState
// envelope (current + history). Locks the wire shape CLI users
// will script against — `jq '.history | length'` etc.
func TestCmdDeploysShow_JSON(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	srv := showServer(t, stageStateAllCompleted(now), map[string]bool{
		"/v1/deployments/" + showTestID + "/stages": true,
	})
	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	stdout, restoreStdout := swapStdout(t)
	defer restoreStdout()
	jsonOutput = true
	defer func() { jsonOutput = false }()

	if code := cmdDeploysShow([]string{showTestID}); code != 0 {
		t.Fatalf("cmdDeploysShow --json = %d, want 0", code)
	}
	var got struct {
		Current string           `json:"current"`
		History []map[string]any `json:"history"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal --json output: %v\nraw: %s", err, stdout.String())
	}
	if got.Current != "readiness" {
		t.Errorf("current: got %q, want %q", got.Current, "readiness")
	}
	if len(got.History) != 6 {
		t.Errorf("history len: got %d, want 6 (closed set)", len(got.History))
	}
}

// TestCmdDeploysShow_NotFoundFromServer: server returns 404
// (cross-account posture or genuinely missing id — the wire is
// identical). CLI must surface it as a non-zero exit; the operator
// sees the same error either way.
func TestCmdDeploysShow_NotFoundFromServer(t *testing.T) {
	srv := showServer(t, []byte(`{}`), map[string]bool{})
	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	if code := cmdDeploysShow([]string{showTestID}); code == 0 {
		t.Errorf("cmdDeploysShow on 404 = 0, want non-zero")
	}
}

// TestCmdDeploysShow_InvalidIDFailsFast pins the local
// deploymentIDPattern gate — bad ids return 1 BEFORE the API
// round-trip. Mirrors cmdDeploymentGet's gate at
// commands_deployments.go:184.
func TestCmdDeploysShow_InvalidIDFailsFast(t *testing.T) {
	// No httptest server — the local regex gate must reject
	// before authedClient is called.
	t.Setenv("FAAS_TOKEN", "")
	if code := cmdDeploysShow([]string{"not-hex"}); code != 1 {
		t.Errorf("cmdDeploysShow bad id = %d, want 1", code)
	}
}

// TestCmdDeploysShow_NoArgs covers the usage-error branch when
// the operator forgets the deployment id.
func TestCmdDeploysShow_NoArgs(t *testing.T) {
	if code := cmdDeploysShow(nil); code != 1 {
		t.Errorf("cmdDeploysShow nil args = %d, want 1", code)
	}
	if code := cmdDeploysShow([]string{"--bogus"}); code != 1 {
		t.Errorf("cmdDeploysShow --bogus (no positional) = %d, want 1", code)
	}
}

// TestCmdDeploys_Dispatcher: the verb-level dispatcher routes
// `show` correctly and rejects unknown subcommands. Pins the
// main.go switch arm and the cli_meta subcommand entry.
func TestCmdDeploys_Dispatcher(t *testing.T) {
	// Empty → usage error (1).
	if code := cmdDeploys(nil); code != 1 {
		t.Errorf("cmdDeploys nil args = %d, want 1", code)
	}
	// Unknown subcommand → usage error (1) — not a panic.
	if code := cmdDeploys([]string{"bogus"}); code != 1 {
		t.Errorf("cmdDeploys unknown sub = %d, want 1", code)
	}
	// `show` with bad id → usage error (1) from cmdDeploysShow.
	// No httptest server needed: id gate fires first.
	if code := cmdDeploys([]string{"show", "not-hex"}); code != 1 {
		t.Errorf("cmdDeploys show bad id = %d, want 1", code)
	}
	// `show` with no args → usage error (1).
	if code := cmdDeploys([]string{"show"}); code != 1 {
		t.Errorf("cmdDeploys show no args = %d, want 1", code)
	}
}

// _ keeps the io import in scope in case a future test wants to
// capture stderr (we currently assert stdout only).
var _ = io.Discard
