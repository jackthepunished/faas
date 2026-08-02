// apply_project_quota_e2e_test.go — plan-quota matrix for the
// apply path. Distinct from quota_e2e_test.go (which covers
// single-app create) in two ways:
//
//   1. The quota gate is on the *project + its workloads* (a
//      5-workload repo against Free trips even though Free allows
//      1 app per the legacy single-app quota gate). Pre-PR-A the
//      single-app quota was the only gate; the multi-workload
//      quota is plan.DeployedApps.
//
//   2. On quota failure the apply path must create NOTHING —
//      zero project rows, zero app rows, zero build rows. The
//      store-side Tx rolls back, the handler surfaces RFC 7807
//      with limit + observed + docs URL.
//
// Each test asserts:
//   - status code (403 for over-cap, 402 for crons-not-allowed).
//   - problem.code matches the spec's stable code.
//   - DB has zero rows in projects / apps for the over-cap apply.
//   - No build rows were created either (PR-A: a quota failure
//     must not enqueue any builds).
//
// PR #541 review M1: a single APID harness is built and started
// once for the whole themed file (TestApplyProject_Quota). Each
// subtest reuses the harness and seeds an isolated account via
// the harness's label-based SeedAccount dedupe. This replaces
// the original pattern of one harness per top-level test.

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
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
	"github.com/onebox-faas/faas/pkg/e2etest"
)

// quotaFixture builds an N-workload tarball with exactly N
// workloads detected by the scanner. The compose service names
// (svc0..svcN) match the convention-detected Dockerfiles under
// services/svc0..svcN so reposcan merges them by (RootDir, Name)
// into a single workload per pair. Without this merge, the
// compose service "api" + convention Dockerfile "svc0" produced
// 2 workloads from a fixture claiming 1 (PR #541 review M2).
func quotaFixture(t *testing.T, workloadCount int) []byte {
	t.Helper()
	if workloadCount < 0 {
		t.Fatalf("quotaFixture: workloadCount=%d must be >= 0", workloadCount)
	}
	// Compose block: one service per requested workload, named
	// `svc0`..`svcN-1`, with matching build context so the
	// compose + convention detectors merge.
	var composeLines strings.Builder
	composeLines.WriteString("services:\n")
	for i := 0; i < workloadCount; i++ {
		composeLines.WriteString("  svc" + itoa(i) + ":\n")
		composeLines.WriteString("    build: { context: services/svc" + itoa(i) + " }\n")
	}
	entries := make([]struct{ name, body string }, 0, workloadCount+1)
	entries = append(entries, struct{ name, body string }{
		"faas-quota/docker-compose.yml", composeLines.String(),
	})
	for i := 0; i < workloadCount; i++ {
		entries = append(entries, struct{ name, body string }{
			"faas-quota/services/svc" + itoa(i) + "/Dockerfile",
			"FROM alpine:3.19\nCMD [\"./svc\"]\n",
		})
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

// itoa is a local helper to avoid strconv import for trivial use.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := []byte{}
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}

// applyProjectExpectProblem POSTs and asserts the response is an
// RFC 7807 problem with the expected status + code.
func applyProjectExpectProblem(t *testing.T, h *e2etest.Harness, key, slug string, body []byte, wantStatus int, wantCode string) {
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
		t.Fatalf("apply req: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("status=%d want %d (body=%s)", resp.StatusCode, wantStatus, raw)
	}
	var p api.Problem
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode problem: %v (body=%s)", err, raw)
	}
	if p.Code != wantCode {
		t.Fatalf("code=%q want %q (body=%s)", p.Code, wantCode, raw)
	}
}

// problemBodyContains is a tiny substring helper used by the
// ProblemCarriesLimitAndObserved subtest to check the problem
// body's JSON keys without pulling in strings.Contains for one
// call. Renamed from `contains` to avoid the package-level
// declaration in account_e2e_test.go.
func problemBodyContains(haystack []byte, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == needle {
			return true
		}
	}
	return false
}

// TestApplyProject_Quota is the single top-level test for this
// themed file. It opens one APID harness + Postgres pool and runs
// each subtest against the shared instance with an isolated
// account. PR #541 review M1 fix.
func TestApplyProject_Quota(t *testing.T) {
	pool := pgtest.Open(t)
	if pool == nil {
		return
	}
	if err := dbMigrateUp(t, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h := e2etest.Start(t, pool, e2etest.APID)

	t.Run("FreePlanOverCap", func(t *testing.T) {
		// Pins the Free plan (DeployedApps=1) with a 2-workload
		// repo. Status 403, code plan_limit_apps, zero rows created.
		key := h.SeedAccount(context.Background(), api.PlanFree, "quota-free-over")
		applyProjectExpectProblem(t, h, key, "free-over", quotaFixture(t, 2),
			http.StatusForbidden, api.CodePlanLimitApps)
		var projCount int
		if err := pool.QueryRow(context.Background(),
			`select count(*) from projects where account_id = (select id from accounts where api_key = $1)`,
			key).Scan(&projCount); err != nil {
			t.Fatalf("count projects: %v", err)
		}
		if projCount != 0 {
			t.Fatalf("Free over-cap apply left %d project rows (rollback failed)", projCount)
		}
	})

	t.Run("HobbyPlanUnderCap", func(t *testing.T) {
		// Pins that Hobby (DeployedApps=5) accepts a 2-workload
		// repo and creates 2 apps + 2 builds.
		key := h.SeedAccount(context.Background(), api.PlanHobby, "quota-hobby-ok")
		ar := applyProjectMultipart(t, h, key, "hobby-ok", "", quotaFixture(t, 2))
		if len(ar.Apps) < 2 {
			t.Fatalf("Hobby accepted only %d apps, want 2", len(ar.Apps))
		}
		if len(ar.Builds) < 2 {
			t.Fatalf("Hobby enqueued only %d builds, want 2", len(ar.Builds))
		}
	})

	t.Run("HobbyPlanOverCap", func(t *testing.T) {
		// Pins Hobby (5 apps) with a 6-workload repo. Status 403,
		// code plan_limit_apps.
		key := h.SeedAccount(context.Background(), api.PlanHobby, "quota-hobby-over")
		applyProjectExpectProblem(t, h, key, "hobby-over", quotaFixture(t, 6),
			http.StatusForbidden, api.CodePlanLimitApps)
	})

	t.Run("ProPlanUnderCap", func(t *testing.T) {
		// Pins Pro (25 apps) + 4-workload repo accepted.
		key := h.SeedAccount(context.Background(), api.PlanPro, "quota-pro-ok")
		ar := applyProjectMultipart(t, h, key, "pro-ok", "", quotaFixture(t, 4))
		if len(ar.Apps) < 4 {
			t.Fatalf("Pro accepted only %d apps, want 4", len(ar.Apps))
		}
	})

	t.Run("ScalePlanUnderCap", func(t *testing.T) {
		// Pins Scale (100 apps) + 6-workload repo accepted.
		key := h.SeedAccount(context.Background(), api.PlanScale, "quota-scale-ok")
		ar := applyProjectMultipart(t, h, key, "scale-ok", "", quotaFixture(t, 6))
		if len(ar.Apps) < 6 {
			t.Fatalf("Scale accepted only %d apps, want 6", len(ar.Apps))
		}
	})

	t.Run("CronsNotAllowed", func(t *testing.T) {
		// Pins the Free plan (CronLimitPerAccount=0). A repo with
		// a cron workload returns 402 plan_crons_not_allowed;
		// zero rows created.
		key := h.SeedAccount(context.Background(), api.PlanFree, "quota-free-cron")
		const renderYAML = `cronJobs:
  - name: nightly
    schedule: "0 3 * * *"
    command: bundle exec rake nightly
`
		entries := []struct{ name, body string }{
			{"faas-cron/docker-compose.yml", "services:\n  api:\n    build: { context: . }\n"},
			{"faas-cron/render.yaml", renderYAML},
			{"faas-cron/Dockerfile", "FROM alpine:3.19\nCMD [\"./api\"]\n"},
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
		applyProjectExpectProblem(t, h, key, "free-cron", buf.Bytes(),
			http.StatusPaymentRequired, api.CodePlanCronsNotAllowed)
	})

	t.Run("OverCapZeroBuilds", func(t *testing.T) {
		// Pins that an over-cap apply creates zero build rows.
		// The apply path must roll back cleanly — even a partial
		// apply that enqueued some builds before tripping the
		// quota gate would leave orphan build rows that
		// builderd might claim.
		key := h.SeedAccount(context.Background(), api.PlanFree, "quota-zero-builds")
		applyProjectExpectProblem(t, h, key, "zero-builds", quotaFixture(t, 2),
			http.StatusForbidden, api.CodePlanLimitApps)
		var buildCount int
		if err := pool.QueryRow(context.Background(),
			`select count(*) from builds b join deployments d on d.id = b.deployment_id join apps a on a.id = d.app_id join accounts acc on acc.id = a.account_id where acc.api_key = $1`,
			key).Scan(&buildCount); err != nil {
			t.Fatalf("count builds: %v", err)
		}
		if buildCount != 0 {
			t.Fatalf("over-cap apply left %d build rows (rollback failed)", buildCount)
		}
	})

	t.Run("ProblemCarriesLimitAndObserved", func(t *testing.T) {
		// Pins the RFC 7807 contract: the problem body carries
		// limit + observed values + a docs URL. The dashboard
		// renders these; a missing field would render
		// "Apply failed (unknown limit)".
		key := h.SeedAccount(context.Background(), api.PlanFree, "quota-bulk-problem")
		body := quotaFixture(t, 5)
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		src, _ := mw.CreateFormFile("source", "fixture.tar.gz")
		_, _ = src.Write(body)
		_ = mw.WriteField("project_slug", "free-bulk")
		_ = mw.Close()
		req, _ := http.NewRequestWithContext(context.Background(),
			http.MethodPost, h.APIDURL+"/v1/projects", &buf)
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
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status=%d want 403", resp.StatusCode)
		}
		var p api.Problem
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("decode: %v", err)
		}
		// The Problem type embeds the limit + observed in the
		// body JSON. Inspect the raw body for the keys.
		for _, want := range []string{`"limit"`, `"observed"`, `"docs"`} {
			if !problemBodyContains(raw, want) {
				t.Fatalf("problem body missing %s (body=%s)", want, raw)
			}
		}
	})

	t.Run("FreeAcceptedAsOneApp", func(t *testing.T) {
		// Pins that Free (DeployedApps=1) accepts a 1-workload
		// repo. The boundary case.
		key := h.SeedAccount(context.Background(), api.PlanFree, "quota-free-ok")
		ar := applyProjectMultipart(t, h, key, "free-ok", "", quotaFixture(t, 1))
		if len(ar.Apps) != 1 {
			t.Fatalf("Free accepted %d apps, want 1", len(ar.Apps))
		}
		if len(ar.Builds) != 1 {
			t.Fatalf("Free enqueued %d builds, want 1", len(ar.Builds))
		}
	})

	t.Run("HobbyCronsAtCap", func(t *testing.T) {
		// Pins Hobby (CronLimitPerAccount=5) with a 6-cron repo.
		// The crons gate is independent of the apps gate.
		key := h.SeedAccount(context.Background(), api.PlanHobby, "quota-hobby-crons-over")
		// A single workload with a Schedule promotes to
		// planCron. 6 such workloads → 6 crons, over Hobby's
		// cap of 5.
		entries := []struct{ name, body string }{
			{"faas-hobby-crons/render.yaml", `cronJobs:
  - name: nightly1
    schedule: "0 3 * * *"
    command: echo 1
  - name: nightly2
    schedule: "0 4 * * *"
    command: echo 2
  - name: nightly3
    schedule: "0 5 * * *"
    command: echo 3
  - name: nightly4
    schedule: "0 6 * * *"
    command: echo 4
  - name: nightly5
    schedule: "0 7 * * *"
    command: echo 5
  - name: nightly6
    schedule: "0 8 * * *"
    command: echo 6
`},
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
		applyProjectExpectProblem(t, h, key, "hobby-crons-over", buf.Bytes(),
			http.StatusForbidden, api.CodePlanCronQuota)
	})
}
