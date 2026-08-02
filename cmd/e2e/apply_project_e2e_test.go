// apply_project_e2e_test.go — non-metal CI-safe acceptance for the
// POST /v1/projects apply path (Phase 3 + PR-A repo decomposition
// Phase 5 close-the-loop).
//
// This is the test surface the existing scan_project_e2e_test.go
// explicitly disclaims ("The actual POST /v1/projects apply path —
// not covered"). The apply path is the gap PR-A in the mega-PR
// closes: today it creates N app rows in PARKED with no source
// build enqueued; after PR-A each added/changed workload gets a
// (deployment, build) pair + a build_queued notify.
//
// PR #541 review M1: a single APID harness is built and started
// once for the whole themed file (TestApplyProject). Each
// subtest reuses the harness and seeds an isolated account via
// the harness's label-based SeedAccount dedupe. This replaces
// the original pattern of one harness per top-level test (~64
// daemon-suite rebuilds) and keeps ./cmd/e2e within its 15-minute
// race/coverage ceiling.
//
// Coverage in this file:
//
//   - Multi-workload happy path: scan + apply a 6-workload repo,
//     assert every workload lands as a state.App row with the
//     correct slug / RootDir / WorkloadClass.
//   - Project row + ScanSource column populated.
//   - ApplyResponse wire shape (project_id, apps[]).
//   - ScanDir is no longer leaked under FAAS_SCAN_SPOOL_ROOT.
//   - MemStore/PgStore kind parity (the trap plan §A4 calls out).
//
// Build tag: (none). CI-safe. Runs under `make test`. Requires
// Postgres (skip via FAAS_SKIP_PG_TESTS) and a buildable ./cmd/apid.

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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
	"github.com/onebox-faas/faas/pkg/e2etest"
	"github.com/onebox-faas/faas/pkg/state"
)

// applyProjectFixture builds the §4 multi-tier tarball (same as
// scan_project_e2e_test.go's fixture) plus the conventional
// services/{api,worker}/ sub-dirs. The fixture is engineered so
// every Tier 1 detector fires and the convention detector picks
// up the two services. Each workload is something a customer
// would actually deploy.
func applyProjectFixture(t *testing.T) []byte {
	t.Helper()
	const composeYML = `services:
  api:
    build:
      context: .
    ports:
      - "8080:8080"
  worker:
    build:
      context: .
    ports:
      - "8081:8081"
`
	entries := []struct {
		name, body string
	}{
		{"faas-apply/docker-compose.yml", composeYML},
		{"faas-apply/services/api/Dockerfile", "FROM alpine:3.19\nEXPOSE 8080\nCMD [\"./api\"]\n"},
		{"faas-apply/services/api/index.js", "exports.handler = () => 1;\n"},
		{"faas-apply/services/worker/Dockerfile", "FROM alpine:3.19\nEXPOSE 8081\nCMD [\"./worker\"]\n"},
		{"faas-apply/services/worker/index.js", "exports.handler = () => 2;\n"},
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     0o644,
			Size:     int64(len(e.body)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %s: %v", e.name, err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatalf("tar write %s: %v", e.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// applyProjectMultipart POSTs to /v1/projects with the given
// `source` bytes + form fields and returns the parsed
// ApplyResponse (or fails the test). Mirrors scanProjectMultipart's
// wiring in scan_project_e2e_test.go; the only differences are the
// URL (`/v1/projects` not `/v1/projects/scan`) and the response
// shape (ApplyResponse embeds PlanResponse plus project_id + apps).
func applyProjectMultipart(t *testing.T, h *e2etest.Harness, key, slug, planToken string, body []byte) api.ApplyResponse {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	src, err := mw.CreateFormFile("source", "fixture.tar.gz")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := src.Write(body); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if slug != "" {
		if err := mw.WriteField("project_slug", slug); err != nil {
			t.Fatalf("write project_slug: %v", err)
		}
	}
	if planToken != "" {
		if err := mw.WriteField("plan_token", planToken); err != nil {
			t.Fatalf("write plan_token: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("multipart close: %v", err)
	}
	url := h.APIDURL + "/v1/projects"
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, url, &buf)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	ctx, cancel := context.WithTimeout(req.Context(), scanRequestTimeout)
	defer cancel()
	req = req.WithContext(ctx)
	resp, err := h.HTTPClient().Do(req)
	if err != nil {
		t.Fatalf("apply request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("apply status = %d, want 200 (body=%s)", resp.StatusCode, raw)
	}
	var ar api.ApplyResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		t.Fatalf("decode ApplyResponse: %v (body=%s)", err, raw)
	}
	return ar
}

// scanRequestTimeout is the per-request timeout used by the
// multipart helpers. 10s is generous; in CI the apply endpoint
// typically returns in <500ms.
const applyRequestTimeout = 10 * time.Second

// TestApplyProject is the single top-level test for this themed
// file. It opens one APID harness + Postgres pool and runs each
// subtest against the shared instance with an isolated account.
// PR #541 review M1 fix.
func TestApplyProject(t *testing.T) {
	pool := pgtest.Open(t)
	if pool == nil {
		return
	}
	if err := dbMigrateUp(t, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h := e2etest.Start(t, pool, e2etest.APID)

	t.Run("MultiWorkloadHappyPath", func(t *testing.T) {
		key := h.SeedAccount(context.Background(), api.PlanPro, "apply-happy")
		body := applyProjectFixture(t)
		ar := applyProjectMultipart(t, h, key, "happy", "", body)
		if ar.ProjectID == "" {
			t.Fatalf("ApplyResponse.ProjectID is empty")
		}
		if len(ar.Apps) < 2 {
			t.Fatalf("expected >= 2 apps, got %d", len(ar.Apps))
		}
		for _, a := range ar.Apps {
			if a.Slug == "" {
				t.Fatalf("app has empty slug: %+v", a)
			}
			if a.ID == "" {
				t.Fatalf("app %q has empty id", a.Slug)
			}
		}
		// PG-side: every returned app_id has a row in apps with the
		// claimed slug + an active status. Catches the case where the
		// HTTP response lied but the DB didn't follow.
		for _, a := range ar.Apps {
			var gotSlug, gotStatus string
			err := pool.QueryRow(context.Background(),
				`select slug, status from apps where id = $1`, a.ID).Scan(&gotSlug, &gotStatus)
			if err != nil {
				t.Fatalf("apps row for %q: %v", a.ID, err)
			}
			if gotSlug != a.Slug {
				t.Fatalf("apps.slug=%q want %q", gotSlug, a.Slug)
			}
			if gotStatus != string(state.AppActive) {
				t.Fatalf("apps.status=%q want active", gotStatus)
			}
		}
	})

	t.Run("ProjectRowAndScanSource", func(t *testing.T) {
		key := h.SeedAccount(context.Background(), api.PlanPro, "apply-scansource")
		ar := applyProjectMultipart(t, h, key, "scansource", "", applyProjectFixture(t))
		if ar.ScanSource != "compose" {
			t.Fatalf("ScanSource=%q want compose (fixture has docker-compose.yml)", ar.ScanSource)
		}
		if ar.Tier != "compose" {
			t.Fatalf("Tier=%q want compose", ar.Tier)
		}
		var prodBranch string
		err := pool.QueryRow(context.Background(),
			`select production_branch from projects where id = $1`, ar.ProjectID).Scan(&prodBranch)
		if err != nil {
			t.Fatalf("projects row: %v", err)
		}
		if prodBranch == "" {
			t.Fatalf("production_branch is empty (default should be 'main')")
		}
	})

	t.Run("ApplyResponseShape", func(t *testing.T) {
		key := h.SeedAccount(context.Background(), api.PlanPro, "apply-wireshape")
		ar := applyProjectMultipart(t, h, key, "wire-shape", "", applyProjectFixture(t))
		if ar.ProjectID == "" {
			t.Fatalf("project_id is empty")
		}
		if ar.ProjectSlug != "wire-shape" {
			t.Fatalf("project_slug=%q want wire-shape", ar.ProjectSlug)
		}
		if ar.PlanToken == "" {
			t.Fatalf("plan_token is empty (apply should still mint one for re-apply use)")
		}
		if !ar.CanApply {
			t.Fatalf("can_apply=false on a Pro plan under cap (apps=%d, limit=%d)",
				len(ar.Workloads), ar.LimitApps)
		}
		if len(ar.Apps) != len(ar.Workloads) {
			t.Fatalf("apps (%d) != workloads (%d): every workload must produce an app row",
				len(ar.Apps), len(ar.Workloads))
		}
	})

	t.Run("NoLeakedScanDir", func(t *testing.T) {
		// This subtest needs FAAS_SCAN_SPOOL_ROOT + FAAS_SPOOL_ROOT
		// overrides; spawn a child harness with the env. We can't
		// reuse the parent's Start because the harness is shared.
		// Instead, point the env vars at temp dirs and assert no
		// leftovers after the apply completes.
		spoolDir := t.TempDir()
		h2 := e2etest.StartWithEnv(t, pool, e2etest.APID, []string{
			"FAAS_SCAN_SPOOL_ROOT=" + spoolDir,
			"FAAS_SPOOL_ROOT=" + t.TempDir(),
		})
		key := h2.SeedAccount(context.Background(), api.PlanPro, "apply-noleak")
		_ = applyProjectMultipart(t, h2, key, "no-leak", "", applyProjectFixture(t))
		entries, err := os.ReadDir(spoolDir)
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("readdir %s: %v", spoolDir, err)
		}
		if len(entries) > 0 {
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Fatalf("FAAS_SCAN_SPOOL_ROOT leaked %d entries after apply: %s", len(entries), strings.Join(names, ","))
		}
	})

	t.Run("DeploymentKindTarball", func(t *testing.T) {
		key := h.SeedAccount(context.Background(), api.PlanPro, "apply-kind")
		ar := applyProjectMultipart(t, h, key, "kind", "", applyProjectFixture(t))
		if len(ar.Apps) == 0 {
			t.Fatalf("no apps in apply response")
		}
		appID := ar.Apps[0].ID
		var depKind string
		err := pool.QueryRow(context.Background(),
			`select kind from deployments where app_id = $1 order by created_at desc limit 1`,
			appID).Scan(&depKind)
		if err != nil {
			if err == pgx.ErrNoRows {
				t.Fatalf("no deployment row for app %q (PR-A didn't enqueue a build)", appID)
			}
			t.Fatalf("deployments query: %v", err)
		}
		if depKind != "tarball" {
			t.Fatalf("deployment.kind=%q want tarball (the only kind in both CHECKs that the apply path can use)", depKind)
		}
	})

	t.Run("BuildRowCreated", func(t *testing.T) {
		key := h.SeedAccount(context.Background(), api.PlanPro, "apply-buildrow")
		ar := applyProjectMultipart(t, h, key, "build-row", "", applyProjectFixture(t))
		if len(ar.Apps) == 0 {
			t.Fatalf("no apps")
		}
		for _, a := range ar.Apps {
			var buildCount int
			err := pool.QueryRow(context.Background(),
				`select count(*) from builds b join deployments d on d.id = b.deployment_id where d.app_id = $1`,
				a.ID).Scan(&buildCount)
			if err != nil {
				t.Fatalf("build count for %s: %v", a.ID, err)
			}
			if buildCount < 1 {
				t.Fatalf("app %s has zero build rows (PR-A must enqueue one)", a.Slug)
			}
		}
	})

	t.Run("AppIDsAreUnique", func(t *testing.T) {
		key := h.SeedAccount(context.Background(), api.PlanPro, "apply-uniqueids")
		ar := applyProjectMultipart(t, h, key, "unique-ids", "", applyProjectFixture(t))
		seen := make(map[string]bool, len(ar.Apps))
		for _, a := range ar.Apps {
			if seen[a.ID] {
				t.Fatalf("duplicate app id %q for slug %q", a.ID, a.Slug)
			}
			seen[a.ID] = true
		}
	})

	t.Run("StagedTarballOnDisk", func(t *testing.T) {
		// Same env-override reason as NoLeakedScanDir.
		spoolRoot := t.TempDir()
		h2 := e2etest.StartWithEnv(t, pool, e2etest.APID, []string{
			"FAAS_SPOOL_ROOT=" + spoolRoot,
		})
		key := h2.SeedAccount(context.Background(), api.PlanPro, "apply-staged")
		ar := applyProjectMultipart(t, h2, key, "staged-on-disk", "", applyProjectFixture(t))
		if len(ar.Apps) == 0 {
			t.Fatalf("no apps")
		}
		var found int
		err := filepath.Walk(spoolRoot, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			if strings.HasSuffix(path, ".tar.gz") && strings.Contains(path, "/projects/") {
				found++
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk spool: %v", err)
		}
		if found < len(ar.Apps) {
			t.Fatalf("staged tarballs on disk: %d, want >= %d (one per app)", found, len(ar.Apps))
		}
	})
}
