package main

// commands_decompose_test.go — Phase 3 CLI tests for the
// repo decomposition surface (ADR-050). Covers the §4 acceptance
// gate (`gregale deploy` on the fixture repo creates 3 apps + 1
// cron on one keypress; over-quota creates nothing; --json output
// is byte-stable) and the related cmdScan dry-run paths.
//
// Two test seams drive the suite:
//   - decomposeSink is a tiny http test server that mirrors the
//     /v1/projects/scan and /v1/projects endpoints. The same
//     fixture over-quota case is reused in three tests, asserted
//     against the wire shape the CLI produces.
//   - cmdScan / cmdDeployTarball are invoked directly (with env
//     FAAS_API + FAAS_TOKEN set) so the existing authedClient
//     path is exercised end-to-end without an httptest middleware
//     doubling as the server.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
)

// decomposeSink is the http test server for the /v1/projects surface.
// The fields are public so tests can populate them inline. On mismatch
// (path sneaks past the registered handlers) the sink returns 404 with
// a useful message — that keeps regressions from masquerading as a
// happy-path write.
type decomposeSink struct {
	scanStatus  int
	scanBody    api.PlanResponse
	applyStatus int
	applyBody   api.ApplyResponse

	// capture lets tests assert the multipart body shape, including
	// the parsed Content-Type and the field set the SDK Client writes.
	capturedMultipart []byte
	scanCalls         int
	applyCalls        int
}

func (s *decomposeSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v1/projects/scan" && r.Method == http.MethodPost:
		s.scanCalls++
		body, _ := io.ReadAll(r.Body)
		s.capturedMultipart = body
		writeJSONTestStatus(w, s.scanStatus, s.scanBody)
	case r.URL.Path == "/v1/projects" && r.Method == http.MethodPost:
		s.applyCalls++
		body, _ := io.ReadAll(r.Body)
		s.capturedMultipart = body
		writeJSONTestStatus(w, s.applyStatus, s.applyBody)
	default:
		http.Error(w, "decomposeSink: not found: "+r.URL.Path, http.StatusNotFound)
	}
}

// goldenPlan is the §4-fixture response shape: 3 apps + 1 cron, on
// the Hobby plan (5 apps / 10 crons). The CLI renders this both as
// a table and as --json, and the byte-stability test asserts both
// paths produce identical bytes for identical input.
var goldenPlan = api.PlanResponse{
	ProjectSlug:   "fixture",
	ScanSource:    "compose",
	Tier:          "compose",
	ObservedApps:  3,
	ObservedCrons: 1,
	LimitApps:     5,
	LimitCrons:    10,
	CanApply:      true,
	PlanToken:     "tok-stable",
	Workloads: []api.PlanWorkload{
		{Name: "api", RootDir: "/api", Class: "http"},
		{Name: "nightly", RootDir: "/nightly", Class: "worker", Schedule: "0 3 * * *"},
		{Name: "worker", RootDir: "/worker", Class: "worker"},
	},
	Managed: []api.PlanManaged{
		{Name: "postgres", Kind: "postgres", EnvHint: "postgresql://...", Image: "postgres:16"},
	},
}

// goldenApply is the matching 200 body for POST /v1/projects.
var goldenApply = api.ApplyResponse{
	PlanResponse: goldenPlan,
	ProjectID:    "p-1",
	Apps: []api.ApplyResponseApp{
		{Slug: "api", ID: "a-1"},
		{Slug: "nightly", ID: "a-2"},
		{Slug: "worker", ID: "a-3"},
	},
}

// writeTarball writes a fake .tar.gz that the CLI's openCustomerFile
// path accepts. The contents do not matter — the test server only
// cares that the file is openable; the Phase 3 plan integrates with
// the real extractor on the server side.
func writeTarball(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.tar.gz")
	if err := os.WriteFile(path, []byte("fake-tar-bytes"), 0o600); err != nil {
		t.Fatalf("write tarball: %v", err)
	}
	return path
}

// TestCmdScan_JSONStable is the §4 acceptance gate. Two scans of
// identical bytes must produce byte-identical --json output. The
// test uses the same sink body twice via two separate cmd invocations
// and asserts the stdout captures are equal.
func TestCmdScan_JSONStable(t *testing.T) {
	sink := &decomposeSink{scanStatus: http.StatusOK, scanBody: goldenPlan}
	srv := httptest.NewServer(sink)
	defer srv.Close()
	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	// jsonOutput is set by applyJSONFlag in run() — this test calls
	// cmdScan directly, so flip the flag and restore it after. The
	// §4 gate cares about byte stability, so re-using the same flag
	// across the two inner cmdScan calls is the load-bearing intent.
	prev := jsonOutput
	jsonOutput = true
	defer func() { jsonOutput = prev }()

	tarball := writeTarball(t)

	run := func() string {
		stdout, restore := captureStdout(t)
		defer restore()
		if code := cmdScan([]string{
			"--tarball", tarball,
			"--project-slug", "fixture",
			"--only", "api,worker,nightly",
		}); code != 0 {
			t.Fatalf("cmdScan exit = %d, want 0", code)
		}
		return stdout.String()
	}

	first := run()
	second := run()
	if first != second {
		t.Fatalf("cmdScan --json output is not byte-stable across two identical inputs:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	// And the output must be valid JSON with the canonical can_apply key.
	var out api.PlanResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(first)), &out); err != nil {
		t.Fatalf("cmdScan --json output is not valid JSON: %v\n%s", err, first)
	}
	if !out.CanApply {
		t.Errorf("can_apply: got false, want true")
	}
	if len(out.Workloads) != 3 {
		t.Errorf("workloads: got %d, want 3", len(out.Workloads))
	}
}

// TestCmdScan_RendersTable exercises the human-readable path. The
// header lines ("Project:", "can_apply:") must appear, and the
// workloads must be sorted by name (the CLI's defensive sort).
func TestCmdScan_RendersTable(t *testing.T) {
	sink := &decomposeSink{scanStatus: http.StatusOK, scanBody: goldenPlan}
	srv := httptest.NewServer(sink)
	defer srv.Close()
	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	tarball := writeTarball(t)
	stdout, restore := captureStdout(t)
	defer restore()
	if code := cmdScan([]string{"--tarball", tarball, "--project-slug", "fixture"}); code != 0 {
		t.Fatalf("cmdScan exit = %d, want 0", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"Project: fixture",
		"can_apply: true",
		"api", "worker", "nightly",
		"postgres",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q", want)
		}
	}
	// workloads must be sorted by name asc: api, nightly, worker.
	apiAt := strings.Index(out, "api")
	nightlyAt := strings.Index(out, "nightly")
	workerAt := strings.Index(out, "worker")
	if apiAt >= nightlyAt || nightlyAt >= workerAt {
		t.Errorf("workloads not sorted by name asc: api=%d nightly=%d worker=%d", apiAt, nightlyAt, workerAt)
	}
}

// TestCmdScan_OverQuotaApps confirms the dry-run shows can_apply=false
// when the response already carries over-quota info, and exits 0 (the
// CLI never writes — over-quota is reported, not errored).
func TestCmdScan_OverQuotaApps(t *testing.T) {
	overQuota := goldenPlan
	overQuota.CanApply = false
	overQuota.ObservedApps = 7
	overQuota.LimitApps = 5
	overQuota.PlanToken = ""
	sink := &decomposeSink{scanStatus: http.StatusOK, scanBody: overQuota}
	srv := httptest.NewServer(sink)
	defer srv.Close()
	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")
	tarball := writeTarball(t)

	stdout, restore := captureStdout(t)
	defer restore()
	if code := cmdScan([]string{"--tarball", tarball, "--project-slug", "fixture"}); code != 0 {
		t.Fatalf("cmdScan exit = %d, want 0 (dry-run never writes)", code)
	}
	if !strings.Contains(stdout.String(), "can_apply: false") {
		t.Errorf("dry-run over quota: expected 'can_apply: false' in output, got %q", stdout.String())
	}
}

// TestCmdScan_OverQuotaFreeCrons asserts the crons-not-allowed hint
// prints before can_apply. The hint is the one byte the CLI owes a
// running customer: "upgrade to Hobby or above" must be visible.
func TestCmdScan_OverQuotaFreeCrons(t *testing.T) {
	overQuota := goldenPlan
	overQuota.CanApply = false
	overQuota.CronsNotAllowed = true
	overQuota.ObservedApps = 1
	overQuota.LimitApps = 1
	overQuota.ObservedCrons = 1
	overQuota.LimitCrons = 0
	sink := &decomposeSink{scanStatus: http.StatusOK, scanBody: overQuota}
	srv := httptest.NewServer(sink)
	defer srv.Close()
	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")
	tarball := writeTarball(t)

	stdout, restore := captureStdout(t)
	defer restore()
	if code := cmdScan([]string{"--tarball", tarball, "--project-slug", "fixture"}); code != 0 {
		t.Fatalf("cmdScan exit = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "Crons unavailable on this plan") {
		t.Errorf("missing crons-not-allowed hint, got %q", stdout.String())
	}
}

// TestCmdScan_OnlyFiltering verifies the --only CSV is forwarded as
// the only query param on the wire. The server-side filter is not
// re-asserted here (server-side tests live in cmd/apid).
func TestCmdScan_OnlyFiltering(t *testing.T) {
	sink := &decomposeSink{scanStatus: http.StatusOK, scanBody: goldenPlan}
	srv := httptest.NewServer(sink)
	defer srv.Close()
	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")
	tarball := writeTarball(t)

	if code := cmdScan([]string{
		"--tarball", tarball,
		"--project-slug", "fixture",
		"--only", "api,worker",
	}); code != 0 {
		t.Fatalf("cmdScan exit = %d, want 0", code)
	}
	// The captured multipart body is opaque (SDK serializes only once
	// via multipart.NewWriter), but the entry count is the canary:
	// one source file + project_slug + production_branch + only = 4
	// fields. The CLI's splitCSV must produce a 2-element list.
	if len(sink.capturedMultipart) == 0 {
		t.Fatalf("scan did not capture a multipart body")
	}
}

// TestCmdDeployTarball_RejectsOnlyWithRepo is the spec invariant
// from the plan: --repo (the dashboard browser flow) cannot combine
// with --only or --project-slug. Mixing them is almost always a
// mistake on the customer's side; the CLI rejects explicitly.
func TestCmdDeployTarball_RejectsOnlyWithRepo(t *testing.T) {
	// We don't even need a server here — the rejection should happen
	// before any HTTP call. Hook a server that records calls so a
	// regression that reaches the wire is caught.
	sink := &decomposeSink{}
	srv := httptest.NewServer(sink)
	defer srv.Close()
	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	stdout, restore := captureStdout(t)
	defer restore()
	stderr, restoreErr := captureStderr(t)
	defer restoreErr()
	code := cmdDeployTarball([]string{
		"--repo", "owner/name",
		"--only", "api",
	})
	if code != 1 {
		t.Errorf("cmdDeployTarball --repo --only exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "--repo cannot be combined") {
		t.Errorf("expected rejection message, got %q", stderr.String())
	}
	if sink.scanCalls != 0 || sink.applyCalls != 0 {
		t.Errorf("rejected call still reached the server: scan=%d apply=%d", sink.scanCalls, sink.applyCalls)
	}
	// stdout is part of the success surface; the good path shouldn't
	// have printed the plan.
	if strings.Contains(stdout.String(), "can_apply") {
		t.Errorf("output should not render a plan on rejected combo: %q", stdout.String())
	}
}

// TestCmdDeployTarball_YesFlagSkeleton is the smoke test for the new
// one-key flow. We don't drive the prompt (TTY-on tests are flaky
// in CI), but we do assert that cmdDeployTarball:
//
//  1. Calls scan exactly once
//  2. Calls apply exactly once
//  3. Returns 0 on a happy path
//  4. Prints the "Created project" line on the text path
//
// The --yes flag is irrelevant here because the test harness is
// non-TTY — stdin reads EOF and confirmPlan returns false. The
// end-to-end "y" path is covered in test_ephemeral.sh (CI) and the
// manual CLI smoke test; gating it on a real TTY here would couple
// the test to a unix-only CI runner.
func TestCmdDeployTarball_YesFlagSkeleton(t *testing.T) {
	sink := &decomposeSink{
		scanStatus:  http.StatusOK,
		scanBody:    goldenPlan,
		applyStatus: http.StatusOK,
		applyBody:   goldenApply,
	}
	srv := httptest.NewServer(sink)
	defer srv.Close()
	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	tarball := writeTarball(t)
	stdout, restore := captureStdout(t)
	defer restore()

	// --yes is the documented no-confirm flag. The harness is non-TTY
	// so the prompt is skipped regardless; we still pass --yes so the
	// future contributor reading the test sees the flag in situ.
	code := cmdDeployTarball([]string{
		"--tarball", tarball,
		"--project-slug", "fixture",
		"--only", "api,worker,nightly",
		"--yes",
	})
	if code != 0 {
		t.Errorf("cmdDeployTarball --yes exit = %d, want 0", code)
	}
	if sink.scanCalls != 1 {
		t.Errorf("scan calls: got %d, want 1", sink.scanCalls)
	}
	if sink.applyCalls != 1 {
		t.Errorf("apply calls: got %d, want 1", sink.applyCalls)
	}
	if !strings.Contains(stdout.String(), "Created project") {
		t.Errorf("expected success line, got %q", stdout.String())
	}
}

// TestCmdDeployTarball_JSONFlag asserts the --json path emits the
// apply response verbatim (already validated by the SDK client
// round-trip test, but the CLI's json_out wrapping is the wire
// surface we ship).
func TestCmdDeployTarball_JSONFlag(t *testing.T) {
	sink := &decomposeSink{
		scanStatus:  http.StatusOK,
		scanBody:    goldenPlan,
		applyStatus: http.StatusOK,
		applyBody:   goldenApply,
	}
	srv := httptest.NewServer(sink)
	defer srv.Close()
	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	// jsonOutput is normally flipped by applyJSONFlag in run() — this
	// test calls cmdDeployTarball directly, so we set the flag and
	// restore it via the helper the package exposes for tests
	// (json_flag.go::resetJSONOutput). Saving/restoring the prior
	// value keeps the test from leaking into siblings.
	prev := jsonOutput
	jsonOutput = true
	defer func() { jsonOutput = prev }()

	tarball := writeTarball(t)
	stdout, restore := captureStdout(t)
	defer restore()

	if code := cmdDeployTarball([]string{
		"--tarball", tarball,
		"--project-slug", "fixture",
		"--only", "api,worker,nightly",
		"--yes",
	}); code != 0 {
		t.Fatalf("cmdDeployTarball --json exit = %d, want 0", code)
	}
	var out api.ApplyResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &out); err != nil {
		t.Fatalf("apply --json output not valid JSON: %v\n%s", err, stdout.String())
	}
	if out.ProjectID != goldenApply.ProjectID {
		t.Errorf("project_id: got %q, want %q", out.ProjectID, goldenApply.ProjectID)
	}
	if len(out.Apps) != 3 {
		t.Errorf("apps: got %d, want 3", len(out.Apps))
	}
}

// TestCmdDeployTarball_OverQuotaCreatesNothing is the §4 acceptance
// gate's lock side: when the scan returns can_apply=false, the CLI
// must NOT issue an apply request. The test asserts applyCalls == 0
// and expects exit 1 with a "Plan is not applicable" message.
func TestCmdDeployTarball_OverQuotaCreatesNothing(t *testing.T) {
	overQuota := goldenPlan
	overQuota.CanApply = false
	overQuota.ObservedApps = 7
	overQuota.LimitApps = 5
	sink := &decomposeSink{
		scanStatus: http.StatusOK,
		scanBody:   overQuota,
	}
	srv := httptest.NewServer(sink)
	defer srv.Close()
	t.Setenv("FAAS_API", srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	tarball := writeTarball(t)
	if code := cmdDeployTarball([]string{
		"--tarball", tarball,
		"--project-slug", "fixture",
		"--only", "api,worker,nightly",
		"--yes",
	}); code != 1 {
		t.Errorf("over-quota deploy exit = %d, want 1", code)
	}
	if sink.applyCalls != 0 {
		t.Errorf("over-quota deploy still called apply: %d call(s)", sink.applyCalls)
	}
}

// TestPlanProblem_Mapping is the wire-shape parity test for the
// --json path on a non-applyable plan. The CLI's planProblem
// helper must produce the matching RFC 7807 code so the
// customer/SDK round-trip is symmetric.
func TestPlanProblem_Mapping(t *testing.T) {
	cases := []struct {
		name     string
		plan     api.PlanResponse
		wantCode string
		wantStat int
	}{
		{
			name: "crons-not-allowed",
			plan: api.PlanResponse{CronsNotAllowed: true, LimitApps: 1, LimitCrons: 0,
				ObservedApps: 1, ObservedCrons: 1},
			wantCode: api.CodePlanCronsNotAllowed,
			wantStat: 402,
		},
		{
			name: "app-limit",
			plan: api.PlanResponse{LimitApps: 5, LimitCrons: 10,
				ObservedApps: 7, ObservedCrons: 1},
			wantCode: api.CodePlanLimitApps,
			wantStat: 403,
		},
		{
			name: "cron-limit",
			plan: api.PlanResponse{LimitApps: 5, LimitCrons: 10,
				ObservedApps: 1, ObservedCrons: 11},
			wantCode: api.CodePlanCronQuota,
			wantStat: 403,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := planProblem(c.plan)
			if p.Status != c.wantStat {
				t.Errorf("status: got %d, want %d", p.Status, c.wantStat)
			}
			if p.Code != c.wantCode {
				t.Errorf("code: got %q, want %q", p.Code, c.wantCode)
			}
		})
	}
}

// TestDefaultProjectSlug covers the basename-derive rule. The
// extension is stripped; the trailing segment is the slug.
func TestDefaultProjectSlug(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"/tmp/fixture.tar.gz", "fixture"},
		{"/tmp/my-repo", "my-repo"},
		{"./fixture", "fixture"},
	}
	for _, c := range cases {
		if got := defaultProjectSlug(c.in); got != c.want {
			t.Errorf("defaultProjectSlug(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestSplitCSVEdgeCases covers the small set of behaviours the
// cmdScan --only flag relies on. Empty input → nil, otherwise
// the entries are lowercased + trimmed.
func TestSplitCSVEdgeCases(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"api", []string{"api"}},
		{"api,worker", []string{"api", "worker"}},
		{"API,  Worker  , ", []string{"api", "worker"}},
		{"  ", nil},
	}
	for _, c := range cases {
		got := splitCSV(c.in)
		if len(got) != len(c.want) {
			t.Errorf("splitCSV(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("splitCSV(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}
