// scan_project_e2e_test.go — non-metal CI-safe acceptance for the
// Phase 3 wire contract on POST /v1/projects/scan.
//
// What this test pins
//
//   - The end-to-end pipeline that takes a customer tarball at the
//     wire surface and produces a `PlanResponse` whose shape
//     matches pkg/api.PlanResponse. This is the bridge the unit
//     suite (`pkg/reposcan/scan_test.go`) cannot reach: the
//     handler does multipart parse → spool → validate →
//     extract → reposcan.Scan → quota gate → plan_token. Each
//     step has its own unit tests; only an httptest+Postgres
//     harness exercises them together.
//
//   - The repo-decomposition §4 fixture expectation: a single
//     tarball carrying compose + Procfile + fly.toml + a
//     convention dir produces a 6-entry / 7-unique-name plan
//     (api from compose, web/worker/cron from Procfile,
//     faas-fixture from fly.toml, services/api + services/worker
//     from convention; the Procfile `worker` and compose `worker`
//     are distinct entries under the (RootDir, Name) merge key
//     and both emit), one managed entry (postgres db),
//     tier=compose, can_apply=true on the Pro plan (5 unique
//     apps under the 25 cap, 1 cron under the 20 cap).
//
//   - The over-quota gate: Free plan returns `can_apply=false`
//     and `crons_not_allowed=true` for the same fixture (Free
//     cron cap is 0; the Procfile `cron:` line trips the gate).
//
//   - The --only filter: posting `only=web,api` produces a
//     2-workload plan while the managed entry remains visible
//     (--only does NOT filter managed; spec §4).
//
// What this test deliberately does NOT cover
//
//   - The actual `POST /v1/projects` apply path — covered by
//     `signed_deploy_e2e_test.go` for the trusted-signer gate and
//     by `quota_e2e_test.go` for the limit envelope. Driving the
//     apply path through here would couple two large surfaces in
//     one test and lose focus.
//
//   - The metal side (vmmd, schedd, gatewayd, imaged, meterd,
//     builderd) — not started. The scan endpoint never wakes an
//     instance; only apid runs.
//
// Build tag: (none). CI-safe. Requires Postgres (skip via
// FAAS_SKIP_PG_TESTS or when pgtest.Open returns nil).

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
	"sort"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
	"github.com/onebox-faas/faas/pkg/e2etest"
)

// scanProjectFixture returns the bytes of a multi-tier customer
// tarball. The shape mirrors a `git archive` of a real backend
// monorepo: one top-level prefix, three Tier 1 sources
// (compose + Procfile + fly.toml), two Tier 3 convention members
// (services/{api,worker}/Dockerfile). The total compressed size
// is well under the smallest plan's 100 MB cap (memory:
// pkg/api/limits.go SourceTarballMaxMB[PlanFree] = 100).
func scanProjectFixture(t *testing.T) []byte {
	t.Helper()
	const composeYML = `services:
  api:
    build:
      context: services/api
    environment:
      DATABASE_URL: postgres://localhost/api
      REDIS_URL: redis://localhost:6379
    ports:
      - "8080:8080"
  worker:
    build:
      context: services/worker
    environment:
      DATABASE_URL: postgres://localhost/api
      REDIS_URL: redis://localhost:6379
  db:
    image: postgres:16-alpine
    environment:
      POSTGRES_PASSWORD: example
`
	const procfile = `web:    bundle exec rails server -p $PORT
worker: bundle exec sidekiq
cron:   bundle exec rake nightly
release: bundle exec rake assets:precompile
`
	const flyTOML = `app = "faas-fixture"
primary_region = "fra"

[build]

[env]
  PORT = "8080"

[[services]]
  internal_port = 8080
  protocol = "tcp"
  [[services.ports]]
    port = 80
    handlers = ["http"]
`
	files := map[string]string{
		"faas-fixture/docker-compose.yml":         composeYML,
		"faas-fixture/Procfile":                   procfile,
		"faas-fixture/fly.toml":                   flyTOML,
		"faas-fixture/services/api/Dockerfile":    "FROM alpine:3.19\nEXPOSE 8080\nCMD [\"./api\"]\n",
		"faas-fixture/services/worker/Dockerfile": "FROM alpine:3.19\nCMD [\"./worker\"]\n",
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %s: %v", name, err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("tar write %s: %v", name, err)
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

// scanProjectMultipart POSTs to /v1/projects/scan with the given
// `source` bytes + form fields and returns the parsed PlanResponse
// (or fails the test). Multipart wiring mirrors the wire contract
// documented at cmd/apid/scan_service.go:461-525: field `source`
// is the tarball; `project_slug` is the kebab identifier; `only`
// is the comma-separated workload filter (empty string omits).
func scanProjectMultipart(t *testing.T, h *e2etest.Harness, key, slug, only string, body []byte) api.PlanResponse {
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
	if only != "" {
		if err := mw.WriteField("only", only); err != nil {
			t.Fatalf("write only: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("multipart close: %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, h.APIDURL+"/v1/projects/scan", &buf)
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
		t.Fatalf("scan request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scan status = %d, want 200 (body=%s)", resp.StatusCode, raw)
	}
	var plan api.PlanResponse
	if err := json.Unmarshal(raw, &plan); err != nil {
		t.Fatalf("decode PlanResponse: %v (body=%s)", err, raw)
	}
	return plan
}

// workloadNames returns the workload names of a plan sorted
// ascending, with duplicates collapsed. The merge key
// (RootDir, Name) keeps a Procfile `worker` (root="") and a
// compose `worker` (root="services/worker") as two distinct
// plan entries — same Name, different key. Most tests want the
// "what user-facing workloads will be created" view, which is
// the unique-name set.
func workloadNames(plan api.PlanResponse) []string {
	seen := make(map[string]bool, len(plan.Workloads))
	for _, w := range plan.Workloads {
		seen[w.Name] = true
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// TestScanProject_MultiTierFixture_UnderQuota pins the §4 fixture
// expectation on the Pro plan (apps under cap, crons under cap).
// The fixture trips every Tier 1 detector (compose + Procfile +
// fly.toml) and the Tier 3 convention detector (services/{api,
// worker}/Dockerfile). The plan surface carries 6 workload
// entries (the Procfile `worker` and compose `worker` are
// distinct under the (RootDir, Name) merge key, so the entries
// for `worker` are emitted twice — once with rootDir="" and once
// with rootDir="services/worker"). By unique Name the plan has
// 7 workloads: api, cron, faas-fixture, services/api,
// services/worker, web, worker. Plus the postgres db managed
// entry and tier=compose.
func TestScanProject_MultiTierFixture_UnderQuota(t *testing.T) {
	t.Parallel()
	pool := pgtest.Open(t)
	if pool == nil {
		return
	}
	if err := db.MigrateUp(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Override the apid spool roots to the per-test tmp so the test
	// never touches /var/spool/faas (no root needed).
	h := e2etest.StartWithEnv(t, pool, e2etest.APID,
		[]string{
			"FAAS_SPOOL_ROOT=" + tmpSpool(t, "source"),
			"FAAS_SCAN_SPOOL_ROOT=" + tmpSpool(t, "scan"),
		})
	key := h.SeedAccount(context.Background(), api.PlanPro, "scan-under-quota")

	plan := scanProjectMultipart(t, h, key, "multi-tier", "", scanProjectFixture(t))

	if plan.ProjectSlug != "multi-tier" {
		t.Errorf("ProjectSlug = %q, want multi-tier", plan.ProjectSlug)
	}
	if plan.Tier != "compose" {
		// Note: scanPlanResponse.Tier is a JSON string; reposcan.Tier
		// is a typed int whose String() returns "compose".
		t.Errorf("Tier = %q, want compose", plan.Tier)
	}
	if !plan.CanApply {
		t.Errorf("CanApply = false on Pro plan (apps under cap, crons under cap); observed=%d limit=%d",
			plan.ObservedApps, plan.LimitApps)
	}
	if plan.CronsNotAllowed {
		t.Errorf("CronsNotAllowed = true on Pro plan; cron cap is %d", plan.LimitCrons)
	}
	wantNames := []string{"api", "cron", "faas-fixture", "services/api", "services/worker", "web", "worker"}
	if got := workloadNames(plan); !equalStrings(got, wantNames) {
		t.Errorf("workload names mismatch:\n got: %v\nwant: %v", got, wantNames)
	}
	// Per-workload field spot checks.
	byName := map[string]api.PlanWorkload{}
	for _, w := range plan.Workloads {
		byName[w.Name] = w
	}
	if w := byName["api"]; w.Class != "unknown" || len(w.Ports) != 1 || w.Ports[0] != 8080 {
		t.Errorf("api workload = %+v; want class=unknown, ports=[8080]", w)
	}
	if w := byName["cron"]; w.Class != "job" {
		t.Errorf("cron workload class = %q, want job", w.Class)
	}
	if w := byName["web"]; w.Class != "http" || len(w.Command) == 0 {
		t.Errorf("web workload = %+v; want class=http, non-empty Command", w)
	}
	// Managed: exactly the postgres db from compose.image:.
	if len(plan.Managed) != 1 || plan.Managed[0].Name != "db" ||
		plan.Managed[0].Kind != "postgres" || plan.Managed[0].EnvHint != "DATABASE_URL" {
		t.Errorf("managed = %+v; want single entry {db, postgres, DATABASE_URL}", plan.Managed)
	}
	// Crons: the Procfile cron row gets a planCron entry with the
	// workload name, schedule "" (Procfile does not carry an
	// expression), and path "/". Workload class is "job".
	var sawCron bool
	for _, c := range plan.Crons {
		if c.WorkloadName == "cron" {
			sawCron = true
			if !c.Enabled || c.Path != "/" {
				t.Errorf("cron planCron = %+v; want Enabled=true Path=/", c)
			}
		}
	}
	if !sawCron {
		t.Errorf("expected planCron entry for workload_name=cron; got: %+v", plan.Crons)
	}
	if plan.PlanToken == "" {
		t.Errorf("PlanToken empty; scan service must mint a token even on the dry-run path")
	}
}

// TestScanProject_FreePlan_CronNotAllowed pins the Free-plan
// cron gate: the same fixture must surface can_apply=false and
// crons_not_allowed=true because the Free plan has CronLimitPerAccount=0
// (per pkg/api/limits.go PlanFree). The app-count check stays
// under cap (5/1), so the only failing axis is the cron gate.
func TestScanProject_FreePlan_CronNotAllowed(t *testing.T) {
	t.Parallel()
	pool := pgtest.Open(t)
	if pool == nil {
		return
	}
	if err := db.MigrateUp(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h := e2etest.StartWithEnv(t, pool, e2etest.APID,
		[]string{
			"FAAS_SPOOL_ROOT=" + tmpSpool(t, "source"),
			"FAAS_SCAN_SPOOL_ROOT=" + tmpSpool(t, "scan"),
		})
	key := h.SeedAccount(context.Background(), api.PlanFree, "scan-free-cron")

	plan := scanProjectMultipart(t, h, key, "multi-tier-free", "", scanProjectFixture(t))

	if plan.CanApply {
		t.Errorf("CanApply = true on Free plan with cron workload; want false")
	}
	if !plan.CronsNotAllowed {
		t.Errorf("CronsNotAllowed = false on Free plan; want true (CronLimitPerAccount=0)")
	}
	// Workloads still discovered (the scan path runs regardless of
	// quota) — the gate is on apply, not scan.
	if len(plan.Workloads) == 0 {
		t.Errorf("Free plan scan returned 0 workloads; expected full plan")
	}
	// Managed still surfaces db (a customer on Free who attempts
	// provision still needs to see what is and isn't going to be
	// created).
	var sawDB bool
	for _, m := range plan.Managed {
		if m.Name == "db" && m.Kind == "postgres" {
			sawDB = true
		}
	}
	if !sawDB {
		t.Errorf("managed db entry missing on Free plan scan; got: %+v", plan.Managed)
	}
}

// TestScanProject_OnlyFilter_ManagedUnaffected pins that the
// --only filter narrows the workloads list but NEVER narrows the
// managed list (spec §4: stateful services the platform will not
// provision must always surface so the customer sees the warning
// signal). The filter accepts a comma-separated workload name
// list; entries not matching are dropped.
func TestScanProject_OnlyFilter_ManagedUnaffected(t *testing.T) {
	t.Parallel()
	pool := pgtest.Open(t)
	if pool == nil {
		return
	}
	if err := db.MigrateUp(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h := e2etest.StartWithEnv(t, pool, e2etest.APID,
		[]string{
			"FAAS_SPOOL_ROOT=" + tmpSpool(t, "source"),
			"FAAS_SCAN_SPOOL_ROOT=" + tmpSpool(t, "scan"),
		})
	key := h.SeedAccount(context.Background(), api.PlanPro, "scan-only-filter")

	// only=web,api narrows to two workloads; the others (cron,
	// faas-fixture, services/api, services/worker, worker) drop.
	plan := scanProjectMultipart(t, h, key, "only-test", "web,api", scanProjectFixture(t))

	wantNames := []string{"api", "web"}
	if got := workloadNames(plan); !equalStrings(got, wantNames) {
		t.Errorf("only-filtered workload names:\n got: %v\nwant: %v", got, wantNames)
	}
	// Managed untouched.
	if len(plan.Managed) != 1 || plan.Managed[0].Name != "db" {
		t.Errorf("only-filtered managed = %+v; want single db entry (--only does not filter managed)", plan.Managed)
	}
}

// TestScanProject_NestedTarballEntries pins the post-#528 fix on
// the wire surface: a tarball whose entries share a common
// top-level prefix (the canonical `git archive` shape — every
// real customer repo produces this) must succeed end-to-end
// through the spool → validate → extract → scan pipeline. Pre-
// fix, the absolute-spool case rejected every nested entry at
// extract.go:215 with `path escape after join rejected`. This
// test wraps the regression in the wire surface so a future
// regression in the multipart-extract-seam pair is caught at CI
// time, not on the EX44 box.
func TestScanProject_NestedTarballEntries(t *testing.T) {
	t.Parallel()
	pool := pgtest.Open(t)
	if pool == nil {
		return
	}
	if err := db.MigrateUp(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Override BOTH spool roots so the test exercises the
	// absolute-path code path (the post-#528 regression shape).
	h := e2etest.StartWithEnv(t, pool, e2etest.APID,
		[]string{
			"FAAS_SPOOL_ROOT=" + tmpSpool(t, "source"),
			"FAAS_SCAN_SPOOL_ROOT=" + tmpSpool(t, "scan"),
		})
	key := h.SeedAccount(context.Background(), api.PlanPro, "scan-nested")

	plan := scanProjectMultipart(t, h, key, "nested", "", scanProjectFixture(t))

	if plan.ProjectSlug != "nested" {
		t.Errorf("ProjectSlug = %q, want nested", plan.ProjectSlug)
	}
	if len(plan.Workloads) == 0 {
		t.Errorf("nested-entry scan returned 0 workloads; the post-#528 fix landed but wire contract regressed")
	}
	// The fixture carries `services/api/Dockerfile` (two levels of
	// nesting under the top-level prefix); the convention detector
	// must surface the corresponding workload. This is the
	// load-bearing wire assertion that the nested-entry seam works.
	var sawServicesAPI bool
	for _, w := range plan.Workloads {
		if w.Name == "services/api" {
			sawServicesAPI = true
		}
	}
	if !sawServicesAPI {
		t.Errorf("expected services/api workload (nested entry seam); workloads = %+v",
			workloadNames(plan))
	}
}

// equalStrings reports whether two sorted string slices contain
// the same entries. Avoids importing reflect in tests.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// tmpSpool returns a path under the test's t.TempDir() so
// apid's spool validation can write into it without root. The
// `label` suffix lets multiple sub-tests share one Harness without
// colliding on the same spool dir (which is fine for parallel
// sub-tests because each gets a fresh tmpdir). Mirrors the
// pattern at cmd/apid/extract_test.go:42-78 where t.Setenv
// stamps FAAS_SCAN_SPOOL_ROOT before the first request.
func tmpSpool(t *testing.T, label string) string {
	t.Helper()
	return t.TempDir() + "/" + label
}

// scanRequestTimeout bounds the multipart upload + scan handler.
// The scan handler itself is sub-second (extract + reposcan on a
// 5-entry fixture), but a cold Mac dev box or a CI runner with
// noisy neighbours can take 5–10s. Matches the 10s bound on
// quota_e2e_test.go:127 (`doReq`).
const scanRequestTimeout = 10 * time.Second
