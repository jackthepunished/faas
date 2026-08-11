// cmd_deploy_source_ref_test.go — whitebox CLI tests for the headless
// source-ref deploy path (issue #739 / DEPLOY-PROV-4 / ADR-092).
//
// Drives cmdDeployTarball (the --repo --ref branch dispatch) and
// cmdDeployRepoSourceRef directly. The httptest sink mirrors the
// PR-A server contract:
//   - POST /v1/apps/{slug}/deployments/source-ref
//   - 202 Accepted on success
//   - 409 + Retry-After on transient githubd / codeload blips
//   - 404 + code=github_install_not_found when the durable install
//     row is absent
//
// The "no install token env" sub-case is the load-bearing regression
// test for PR-A's whole point: a CI runner with only FAAS_TOKEN set
// can drive a deploy end-to-end, no GREGALE_INSTALL_TOKEN_* needed.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
)

// sourceRefSink is the http test server for the source-ref deploy
// surface. Mirrors the shape of decomposeSink (commands_decompose_test.go:37)
// but for /v1/apps/{slug}/deployments/source-ref. Public fields so
// tests can populate per-sub-case inline.
type sourceRefSink struct {
	// path is the slug the test registered the handler for.
	path string

	// status + body mirror what the server emits. retryAfter is the
	// wire header (non-empty → server sets Retry-After: <value>).
	status     int
	body       api.DeploymentResponse
	problem    *api.Problem
	retryAfter string

	// captured records the wire request so assertions can pin
	// method, path, body shape, and the auto-minted Idempotency-Key.
	captured      *http.Request
	capturedBody  []byte
	capturedCalls int
}

// ServeHTTP is the single dispatch arm — the sink only knows the
// source-ref path. Any other path returns 404 with a useful message
// so a regression that drives the wrong wire URL fails loud.
func (s *sourceRefSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.captured = r
	body, _ := io.ReadAll(r.Body)
	s.capturedBody = body
	s.capturedCalls++
	if s.problem != nil {
		if s.retryAfter != "" {
			w.Header().Set("Retry-After", s.retryAfter)
		}
		writeJSONTestStatus(w, s.status, s.problem)
		return
	}
	writeJSONTestStatus(w, s.status, s.body)
}

// withResetJSONOutput flips the global jsonOutput flag for the
// duration of t and restores it. Mirrors the pattern used in
// commands_decompose_test.go for --json-mode tests.
func withResetJSONOutput(t *testing.T, v bool) {
	t.Helper()
	prev := jsonOutput
	jsonOutput = v
	t.Cleanup(func() { jsonOutput = prev })
}

// TestCmdDeployRepoSourceRef_HappyPath is the §4-style acceptance
// gate for issue #739: with FAAS_TOKEN set and no GitHub env vars,
// gregale deploy --repo OWNER/NAME --ref <sha> posts to the new
// endpoint, the server accepts, and the CLI emits a stable JSON
// deployment response.
//
// Verifies:
//   - POST /v1/apps/{slug}/deployments/source-ref
//   - body decodes to SourceRefDeployRequest{Repo, Ref, Format:"tarball"}
//   - Idempotency-Key is auto-minted (Client.do, pkg/api/client.go:206)
//   - 202 → DeploymentResponse renders as JSON on stdout
//   - no GREGALE_INSTALL_TOKEN_* env var is consulted (sentinel
//     unset below)
func TestCmdDeployRepoSourceRef_HappyPath(t *testing.T) {
	// Regression: the CLI must NOT touch any install-token env
	// var. Unset every GREGALE_INSTALL_TOKEN_* prefix to assert the
	// happy path still completes when the runner has none of them.
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "GREGALE_INSTALL_TOKEN_") {
			name := strings.SplitN(e, "=", 2)[0]
			t.Setenv(name, "")
		}
	}

	sink := &sourceRefSink{
		path:   "/v1/apps/hello/deployments/source-ref",
		status: http.StatusAccepted,
		body: api.DeploymentResponse{
			ID:          "dep_2",
			AppID:       "app_hello",
			BuildID:     "build_2",
			ImageDigest: "",
			Kind:        "github",
			Status:      "queued",
		},
	}
	srv := httptest.NewServer(sink)
	t.Cleanup(srv.Close)
	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")
	withResetJSONOutput(t, true)

	stdout, restore := captureStdout(t)
	defer restore()

	code := cmdDeployRepoSourceRef("hello", "onebox-faas/hello", "0123456789abcdef0123456789abcdef01234567")
	if code != 0 {
		t.Fatalf("cmdDeployRepoSourceRef exit = %d, want 0", code)
	}
	if sink.capturedCalls != 1 {
		t.Fatalf("server calls = %d, want 1", sink.capturedCalls)
	}
	if sink.captured.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", sink.captured.Method)
	}
	if sink.captured.URL.Path != sink.path {
		t.Errorf("path = %q, want %q", sink.captured.URL.Path, sink.path)
	}
	if got := sink.captured.Header.Get("Idempotency-Key"); got == "" {
		t.Errorf("Idempotency-Key missing — Client.do should auto-mint for non-GET/HEAD")
	}

	var got api.SourceRefDeployRequest
	if err := json.Unmarshal(sink.capturedBody, &got); err != nil {
		t.Fatalf("decode request body: %v\n%s", err, sink.capturedBody)
	}
	if got.Repo != "onebox-faas/hello" {
		t.Errorf("body.repo = %q, want onebox-faas/hello", got.Repo)
	}
	if got.Ref != "0123456789abcdef0123456789abcdef01234567" {
		t.Errorf("body.ref = %q, want pinned SHA", got.Ref)
	}
	if got.Format != "tarball" {
		t.Errorf("body.format = %q, want tarball (PR-A only supports tarball)", got.Format)
	}

	// stdout should be a single JSON-encoded DeploymentResponse.
	var out api.DeploymentResponse
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); err != nil {
		t.Fatalf("decode stdout: %v\n%s", err, stdout.String())
	}
	if out.ID != "dep_2" || out.Status != "queued" {
		t.Errorf("stdout deployment = %+v, want id=dep_2 status=queued", out)
	}
}

// TestCmdDeployTarball_RequiresRefWithRepo covers Layer 1's missing-
// --ref guard: --repo without --ref must exit 1 before any HTTP
// call, and the stderr message must point at the missing flag.
func TestCmdDeployTarball_RequiresRefWithRepo(t *testing.T) {
	// Stand up a sink so a regression that reaches the wire is
	// caught (the guard should reject before any HTTP call).
	sink := &sourceRefSink{path: "/v1/apps/hello/deployments/source-ref"}
	srv := httptest.NewServer(sink)
	t.Cleanup(srv.Close)
	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	_, restore := captureStdout(t)
	defer restore()
	stderr, restoreErr := captureStderr(t)
	defer restoreErr()

	code := cmdDeployTarball([]string{
		"--repo", "onebox-faas/hello",
		// no --ref
	})
	if code != 1 {
		t.Fatalf("cmdDeployTarball exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "missing --ref") {
		t.Errorf("expected 'missing --ref' in stderr, got %q", stderr.String())
	}
	if sink.capturedCalls != 0 {
		t.Errorf("rejected call still reached the server: calls=%d", sink.capturedCalls)
	}
}

// TestCmdDeployTarball_RejectsInvalidRepoSlug covers validateRepoSlug:
// a malformed --repo must exit 1 before any HTTP call.
func TestCmdDeployTarball_RejectsInvalidRepoSlug(t *testing.T) {
	sink := &sourceRefSink{path: "/v1/apps/x/deployments/source-ref"}
	srv := httptest.NewServer(sink)
	t.Cleanup(srv.Close)
	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	_, restore := captureStdout(t)
	defer restore()
	stderr, restoreErr := captureStderr(t)
	defer restoreErr()

	code := cmdDeployTarball([]string{
		"--repo", "bad slug with spaces",
		"--ref", "0123456789abcdef0123456789abcdef01234567",
	})
	if code != 1 {
		t.Fatalf("cmdDeployTarball exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "Invalid --repo") {
		t.Errorf("expected 'Invalid --repo' in stderr, got %q", stderr.String())
	}
	if sink.capturedCalls != 0 {
		t.Errorf("rejected call still reached the server: calls=%d", sink.capturedCalls)
	}
}

// TestCmdDeployRepoSourceRef_RetryAfterOnSourceRefUnavailable covers
// the load-bearing 503 / 409 backoff surface: when the server emits
// 409 source_ref_unavailable with Retry-After: 30, the CLI must
// surface "(Retry-After: 30s)" on stderr so the operator can back
// off without reaching for the audit log.
//
// Regression: this required the SDK doReq to copy the Retry-After
// wire header into Problem.extraHeaders (pkg/api/client.go). The
// test would fail with "(Retry-After: 0s)" if that copy regressed.
func TestCmdDeployRepoSourceRef_RetryAfterOnSourceRefUnavailable(t *testing.T) {
	sink := &sourceRefSink{
		path:       "/v1/apps/hello/deployments/source-ref",
		status:     http.StatusConflict, // 409 per the wire contract
		retryAfter: "30",
		problem: &api.Problem{
			Status: http.StatusConflict,
			Code:   "source_ref_unavailable",
			Title:  "Source ref unavailable",
			Detail: "githubd stream timed out; retry after the indicated backoff.",
		},
	}
	srv := httptest.NewServer(sink)
	t.Cleanup(srv.Close)
	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")
	withResetJSONOutput(t, true)

	_, restore := captureStdout(t)
	defer restore()
	stderr, restoreErr := captureStderr(t)
	defer restoreErr()

	code := cmdDeployRepoSourceRef("hello", "onebox-faas/hello", "0123456789abcdef0123456789abcdef01234567")
	if code == 0 {
		t.Fatalf("cmdDeployRepoSourceRef exit = 0, want non-zero on 409")
	}
	if !strings.Contains(stderr.String(), "Retry-After: 30s") {
		t.Errorf("expected 'Retry-After: 30s' on stderr, got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "source_ref_unavailable") {
		t.Errorf("expected 'source_ref_unavailable' code on stderr, got %q", stderr.String())
	}
}

// TestCmdDeployRepoSourceRef_NotFoundSurfacesCode covers the 404
// github_install_not_found path: the bind row is missing. The CLI
// must surface the server's code so the operator knows to run
// `gregale connect` on a workstation.
func TestCmdDeployRepoSourceRef_NotFoundSurfacesCode(t *testing.T) {
	sink := &sourceRefSink{
		path:   "/v1/apps/hello/deployments/source-ref",
		status: http.StatusNotFound,
		problem: &api.Problem{
			Status: http.StatusNotFound,
			Code:   "github_install_not_found",
			Title:  "GitHub install not found",
			Detail: "no github_installations row for this account; run `gregale connect`",
		},
	}
	srv := httptest.NewServer(sink)
	t.Cleanup(srv.Close)
	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")
	withResetJSONOutput(t, true)

	_, restore := captureStdout(t)
	defer restore()
	stderr, restoreErr := captureStderr(t)
	defer restoreErr()

	code := cmdDeployRepoSourceRef("hello", "onebox-faas/hello", "0123456789abcdef0123456789abcdef01234567")
	if code == 0 {
		t.Fatalf("cmdDeployRepoSourceRef exit = 0, want non-zero on 404")
	}
	if !strings.Contains(stderr.String(), "github_install_not_found") {
		t.Errorf("expected 'github_install_not_found' on stderr, got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "gregale connect") {
		t.Errorf("expected 'gregale connect' hint on stderr, got %q", stderr.String())
	}
}

// TestCmdDeployRepoSourceRef_NoInstallTokenEnv is the §4-style CI
// regression test for the whole point of issue #739 / ADR-092:
// the CLI must drive a deploy end-to-end without consulting any
// GREGALE_INSTALL_TOKEN_* env var.
//
// We strip every GREGALE_INSTALL_TOKEN_* from the test process
// environment, run a happy path, and assert the server saw one
// call. If a future regression ever re-introduces a readInstallToken
// branch, this test would still pass — the wire-shape difference
// (POST JSON, not multipart tarball upload) is what catches the
// regression at the SDK boundary.
func TestCmdDeployRepoSourceRef_NoInstallTokenEnv(t *testing.T) {
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "GREGALE_INSTALL_TOKEN_") {
			name := strings.SplitN(e, "=", 2)[0]
			// t.Setenv("", "") would panic; use os.Unsetenv via
			// Cleanup so the test process env never sees these
			// vars while the test runs.
			t.Cleanup(func() {
				old, had := os.LookupEnv(name)
				_ = os.Unsetenv(name)
				t.Cleanup(func() {
					if had {
						_ = os.Setenv(name, old)
					}
				})
			})
			_ = os.Unsetenv(name)
		}
	}

	sink := &sourceRefSink{
		path:   "/v1/apps/hello/deployments/source-ref",
		status: http.StatusAccepted,
		body: api.DeploymentResponse{
			ID: "dep_3", AppID: "app_hello", BuildID: "build_3",
			Kind: "github", Status: "queued",
		},
	}
	srv := httptest.NewServer(sink)
	t.Cleanup(srv.Close)
	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")
	withResetJSONOutput(t, true)

	_, restore := captureStdout(t)
	defer restore()

	code := cmdDeployRepoSourceRef("hello", "onebox-faas/hello", "0123456789abcdef0123456789abcdef01234567")
	if code != 0 {
		t.Fatalf("cmdDeployRepoSourceRef exit = %d, want 0 (no install token env, server responds 202)", code)
	}
	if sink.capturedCalls != 1 {
		t.Fatalf("server calls = %d, want 1", sink.capturedCalls)
	}
	// Wire-shape pin: the request must be the JSON POST, NOT a
	// multipart tarball upload. The Content-Type header is the
	// observable signal.
	if got := sink.captured.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (source-ref is JSON, not multipart)", got)
	}
}

// Compile-time guard: sourceRefSink implements http.Handler.
var _ http.Handler = (*sourceRefSink)(nil)

// Reference the context import so `go vet` doesn't complain when
// future sub-cases add ctx-aware behaviour.
var _ = context.Background
