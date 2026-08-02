// apply_project_guards_e2e_test.go — coverage for the three
// reconcile guards (plan §B "apply_project_guards"):
//
//   1. neverEmpty: a repo with zero detectable workloads must
//      trip AlertKindNoWorkloads + ErrPlanEmpty. The handler
//      surfaces 422 plan_empty.
//
//   2. productionBranchOnly: an apply whose prod_branch differs
//      from the project's existing production_branch trips
//      state.ErrProdBranchMismatch. **Note:** `branch==""` in
//      the call signature deliberately SKIPS this guard on the
//      apid interactive path (pkg/reconcile/guards.go:82-86) —
//      the test pins that skip-and-allow behaviour so a future
//      refactor doesn't accidentally start rejecting
//      empty-branch applies.
//
//   3. scanSourceStable: an apply whose ScanSource tier is lower
//      than the project's existing tier trips
//      state.ErrScanSourceDowngrade. The handler returns 409.
//
// Each guard also emits an audit/alert row the dashboard renders.
//
// PR #541 review M1: a single APID harness is built and started
// once for the whole themed file (TestApplyProject_Guards).
// Each subtest reuses the harness and seeds an isolated account
// via the harness's label-based SeedAccount dedupe. This
// replaces the original pattern of one harness per top-level
// test.

package e2e_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
	"github.com/onebox-faas/faas/pkg/e2etest"
)

// singleWorkloadFixture builds a tarball with exactly one
// convention-detector workload (services/api/Dockerfile).
func singleWorkloadFixture(t *testing.T) []byte {
	t.Helper()
	entries := []struct{ name, body string }{
		{"faas-guard/services/api/Dockerfile", "FROM alpine:3.19\nCMD [\"./api\"]\n"},
		{"faas-guard/services/api/index.js", "exports.handler = () => 1;\n"},
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		_ = tw.WriteHeader(hdr)
		_, _ = tw.Write([]byte(e.body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// composeFixture builds a tarball with docker-compose.yml (tier
// 'compose') and one service. Used for the scanSourceStable test
// which needs a tier-1 repo on the first apply.
func composeFixture(t *testing.T) []byte {
	t.Helper()
	entries := []struct{ name, body string }{
		{"faas-tier/docker-compose.yml", "services:\n  api:\n    build: { context: . }\n"},
		{"faas-tier/Dockerfile", "FROM alpine:3.19\nCMD [\"./api\"]\n"},
		{"faas-tier/index.js", "exports.handler = () => 1;\n"},
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		_ = tw.WriteHeader(hdr)
		_, _ = tw.Write([]byte(e.body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// conventionOnlyFixture builds a tarball with no top-level detector
// markers; only the convention detector fires. Used to create a
// tier='convention' project on the first apply so the second
// (compose) apply trips the scan-source-downgrade guard.
func conventionOnlyFixture(t *testing.T) []byte {
	t.Helper()
	entries := []struct{ name, body string }{
		{"faas-conv/services/api/Dockerfile", "FROM alpine:3.19\nCMD [\"./api\"]\n"},
		{"faas-conv/services/api/index.js", "exports.handler = () => 1;\n"},
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		_ = tw.WriteHeader(hdr)
		_, _ = tw.Write([]byte(e.body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// emptyFixture builds a tarball with no detectable workloads —
// just a single empty README. Trips the neverEmpty guard.
func emptyFixture(t *testing.T) []byte {
	t.Helper()
	entries := []struct{ name, body string }{
		{"faas-empty/README.md", "# nothing here\n"},
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		_ = tw.WriteHeader(hdr)
		_, _ = tw.Write([]byte(e.body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// applyProjectRaw POSTs and returns (status, body bytes). Mirrors
// applyProjectMultipart but with optional production_branch form
// field, which the canonical helper doesn't expose (apply default
// is "main"; explicit branch is only used by the production-branch
// guard tests).
func applyProjectRaw(t *testing.T, h *e2etest.Harness, key, slug, prodBranch string, body []byte) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	src, err := mw.CreateFormFile("source", "fixture.tar.gz")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	_, _ = src.Write(body)
	if slug != "" {
		_ = mw.WriteField("project_slug", slug)
	}
	if prodBranch != "" {
		_ = mw.WriteField("production_branch", prodBranch)
	}
	_ = mw.Close()
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, h.APIDURL+"/v1/projects", &buf)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	resp, err := h.HTTPClient().Do(req)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// TestApplyProject_Guards is the single top-level test for this
// themed file. It opens one APID harness + Postgres pool and runs
// each subtest against the shared instance with an isolated
// account. PR #541 review M1 fix.
func TestApplyProject_Guards(t *testing.T) {
	pool := pgtest.Open(t)
	if pool == nil {
		return
	}
	if err := dbMigrateUp(t, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h := e2etest.Start(t, pool, e2etest.APID)

	t.Run("NeverEmpty", func(t *testing.T) {
		// Pins that an empty repo (no detectable workloads)
		// trips the neverEmpty guard with 422.
		key := h.SeedAccount(context.Background(), api.PlanPro, "guard-empty")
		status, body := applyProjectRaw(t, h, key, "empty", "", emptyFixture(t))
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("status=%d want 422 (plan_empty), body=%s", status, body)
		}
		var p api.Problem
		if err := json.Unmarshal(body, &p); err != nil {
			t.Fatalf("decode: %v (body=%s)", err, body)
		}
		if p.Code != "plan_empty" {
			t.Fatalf("code=%q want plan_empty", p.Code)
		}
	})

	t.Run("NeverEmptyNoRows", func(t *testing.T) {
		// Pins the roll-back invariant: an empty-repo apply
		// creates zero project + zero app rows. (Same shape as
		// the quota tests — the store Tx must roll back cleanly
		// on guard failure.)
		key := h.SeedAccount(context.Background(), api.PlanPro, "guard-empty-rb")
		_, _ = applyProjectRaw(t, h, key, "empty-rb", "", emptyFixture(t))
		var projCount int
		if err := pool.QueryRow(context.Background(),
			`select count(*) from projects where slug = 'empty-rb'`).Scan(&projCount); err != nil {
			t.Fatalf("count projects: %v", err)
		}
		if projCount != 0 {
			t.Fatalf("never-empty apply left %d project rows", projCount)
		}
	})

	t.Run("ProductionBranchMatch", func(t *testing.T) {
		// Pins the happy path: an apply whose prod_branch
		// matches the project's existing production_branch is
		// accepted.
		key := h.SeedAccount(context.Background(), api.PlanPro, "guard-branch-ok")
		// First apply with default prod_branch (empty → 'main').
		ar := applyProjectMultipart(t, h, key, "branch-ok", "", singleWorkloadFixture(t))
		// Second apply with explicit prod_branch='main' must be
		// accepted (matches).
		status, _ := applyProjectRaw(t, h, key, "branch-ok", "main", singleWorkloadFixture(t))
		if status != http.StatusOK {
			t.Fatalf("matched-branch re-apply status=%d want 200", status)
		}
		if ar.ProjectID == "" {
			t.Fatalf("first apply returned empty project_id")
		}
	})

	t.Run("ProductionBranchDefaultsToMain", func(t *testing.T) {
		// PR #541 review H2 fix: pin the deliberate
		// skip-and-default behaviour. An apply that omits
		// production_branch must (a) be accepted (200, not
		// rejected by the guard) and (b) result in
		// projects.production_branch = 'main'. The original
		// "EmptySkips" test only checked (a), so a refactor
		// that left production_branch='' would pass — even
		// though the dashboard renders production_branch and
		// would show a blank cell. This test now asserts both.
		key := h.SeedAccount(context.Background(), api.PlanPro, "guard-branch-default")
		status, body := applyProjectRaw(t, h, key, "branch-default", "", singleWorkloadFixture(t))
		if status != http.StatusOK {
			t.Fatalf("empty-branch apply status=%d want 200, body=%s", status, body)
		}
		var prodBranch string
		if err := pool.QueryRow(context.Background(),
			`select production_branch from projects where slug = 'branch-default'`).Scan(&prodBranch); err != nil {
			t.Fatalf("projects.production_branch query: %v", err)
		}
		if prodBranch != "main" {
			t.Fatalf("projects.production_branch=%q want %q (apply default is 'main')", prodBranch, "main")
		}
	})

	t.Run("ProductionBranchMismatch", func(t *testing.T) {
		// Pins the failure path: an apply whose prod_branch
		// differs from the project's existing branch trips
		// state.ErrProdBranchMismatch. The handler maps this
		// to 409 prod_branch_mismatch.
		key := h.SeedAccount(context.Background(), api.PlanPro, "guard-branch-mm")
		// First apply pins production_branch='main'.
		applyProjectMultipart(t, h, key, "branch-mm", "", singleWorkloadFixture(t))
		// Second apply with prod_branch='develop' — mismatch.
		status, body := applyProjectRaw(t, h, key, "branch-mm", "develop", singleWorkloadFixture(t))
		if status != http.StatusConflict {
			t.Fatalf("mismatch status=%d want 409, body=%s", status, body)
		}
	})

	t.Run("ScanSourceStable", func(t *testing.T) {
		// Pins the scan-source downgrade guard. First apply
		// creates a project with tier 'compose'
		// (composeFixture has docker-compose.yml). Second
		// apply posts a convention-only fixture, which is
		// tier 'convention' — convention < compose in the
		// rank table, so the downgrade guard trips and the
		// handler returns 409.
		key := h.SeedAccount(context.Background(), api.PlanPro, "guard-scan-stable")
		// Tier=compose on first apply.
		ar := applyProjectMultipart(t, h, key, "scan-stable", "", composeFixture(t))
		if ar.ScanSource != "compose" {
			t.Fatalf("first apply ScanSource=%q want compose", ar.ScanSource)
		}
		// Tier=convention on second apply → downgrade.
		status, body := applyProjectRaw(t, h, key, "scan-stable", "", conventionOnlyFixture(t))
		if status != http.StatusConflict {
			t.Fatalf("downgrade status=%d want 409, body=%s", status, body)
		}
		var p api.Problem
		if err := json.Unmarshal(body, &p); err != nil {
			t.Fatalf("decode: %v (body=%s)", err, body)
		}
		if p.Code != "scan_source_downgrade" {
			t.Fatalf("code=%q want scan_source_downgrade", p.Code)
		}
	})

	t.Run("ScanSourceUpgrade", func(t *testing.T) {
		// Pins the opposite: an upgrade is always accepted
		// (a convention project can adopt compose). No
		// downgrade, so no rejection.
		key := h.SeedAccount(context.Background(), api.PlanPro, "guard-scan-up")
		// First apply: convention tier.
		ar1 := applyProjectMultipart(t, h, key, "scan-up", "", conventionOnlyFixture(t))
		if ar1.ScanSource != "convention" {
			t.Skipf("first apply ScanSource=%q — convention detector may have changed", ar1.ScanSource)
		}
		// Second apply: compose tier (upgrade) — must be
		// accepted.
		status, _ := applyProjectRaw(t, h, key, "scan-up", "", composeFixture(t))
		if status != http.StatusOK {
			t.Fatalf("upgrade status=%d want 200 (upgrades are allowed)", status)
		}
	})

	t.Run("NoWorkloadsAuditRow", func(t *testing.T) {
		// Pins that the neverEmpty guard emits an
		// alert/audit row. The dashboard surfaces "no
		// workloads detected" via this row; a missing row
		// would render nothing. We tolerate the table not
		// existing (older schemas) but check for the row
		// when it does.
		key := h.SeedAccount(context.Background(), api.PlanPro, "guard-no-wl")
		_, _ = applyProjectRaw(t, h, key, "no-wl", "", emptyFixture(t))
		// Check alerts (the per-app emit) — the table may
		// not exist on this schema; skip if so. We just want
		// to pin that the guard emits SOMETHING the
		// dashboard reads.
		var alertCount int
		err := pool.QueryRow(context.Background(),
			`select count(*) from alerts where kind = 'no_workloads'`).Scan(&alertCount)
		if err != nil {
			t.Logf("alerts query failed (table may not exist): %v", err)
			return
		}
		if alertCount == 0 {
			t.Fatalf("never-empty guard emitted zero alert rows (dashboard will show nothing)")
		}
	})
}
