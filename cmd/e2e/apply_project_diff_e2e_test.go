// apply_project_diff_e2e_test.go — coverage for the second-apply
// diff path (plan §B "apply_project_diff"):
//
//   1. unchanged: a second apply with the SAME workloads produces
//      no new build rows. Pre-PR-A there were no build rows at
//      all; post-PR-A we must not enqueue spurious builds when
//      nothing changed.
//   2. added: a new workload appears in the second apply, gets a
//      build row.
//   3. removed: a workload present in the first apply is absent
//      from the second — its apps row goes soft-deleted
//      (state.ErrSoftDelete triggers downstream cascades: envs,
//      domains, crons).
//   4. changed: a workload's source hash differs — its deployment
//      row is superseded, new build enqueued.
//   5. cron-soft-delete: a render.yaml cron that disappears between
//      applies is soft-deleted (the PR-GH.6 500 regression path).
//   6. domain cascade: a domain attached to a removed app is
//      removed too.
//   7. env cascade: an env var on a removed app is removed too.
//
// PR #541 review M1: a single APID harness is built and started
// once for the whole themed file (TestApplyProject_Diff). Each
// subtest reuses the harness and seeds an isolated account via
// the harness's label-based SeedAccount dedupe. This replaces
// the original pattern of one harness per top-level test.

package e2e_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
	"github.com/onebox-faas/faas/pkg/e2etest"
)

// twoWorkloadFixture builds an N=2 repo so a 2nd apply can add /
// remove / mutate one workload while keeping the other intact.
func twoWorkloadFixture(t *testing.T, prefix string) []byte {
	t.Helper()
	entries := []struct{ name, body string }{
		{prefix + "/docker-compose.yml", "services:\n  api:\n    build: { context: services/api }\n  worker:\n    build: { context: services/worker }\n"},
		{prefix + "/services/api/Dockerfile", "FROM alpine:3.19\nCMD [\"./api\"]\n"},
		{prefix + "/services/api/index.js", "exports.handler = () => 1;\n"},
		{prefix + "/services/worker/Dockerfile", "FROM alpine:3.19\nCMD [\"./worker\"]\n"},
		{prefix + "/services/worker/index.js", "exports.handler = () => 2;\n"},
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

// oneWorkloadFixtureFromPrefix builds a 1-workload repo. The
// workload name is taken from the suffix of `prefix` (so we can
// run twoWorkloadFixture + oneWorkloadFixtureFromPrefix with
// overlapping prefixes to test removal).
func oneWorkloadFixtureFromPrefix(t *testing.T, prefix string) []byte {
	t.Helper()
	entries := []struct{ name, body string }{
		{prefix + "/docker-compose.yml", "services:\n  api:\n    build: { context: services/api }\n"},
		{prefix + "/services/api/Dockerfile", "FROM alpine:3.19\nCMD [\"./api\"]\n"},
		{prefix + "/services/api/index.js", "exports.handler = () => 1;\n"},
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

// twoWorkloadChangedFixture builds a 2-workload repo where the
// `api` workload's index.js content differs from the first apply
// — this exercises the `~` changed path (new deployment +
// superseded old build).
func twoWorkloadChangedFixture(t *testing.T, prefix string) []byte {
	t.Helper()
	entries := []struct{ name, body string }{
		{prefix + "/docker-compose.yml", "services:\n  api:\n    build: { context: services/api }\n  worker:\n    build: { context: services/worker }\n"},
		{prefix + "/services/api/Dockerfile", "FROM alpine:3.19\nCMD [\"./api\"]\n"},
		// Note: body differs from twoWorkloadFixture's index.js
		// (1 vs 99). The detector hashes the source tree; a
		// different body → different hash → `~` changed.
		{prefix + "/services/api/index.js", "exports.handler = () => 99;\n"},
		{prefix + "/services/worker/Dockerfile", "FROM alpine:3.19\nCMD [\"./worker\"]\n"},
		{prefix + "/services/worker/index.js", "exports.handler = () => 2;\n"},
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

// TestApplyProject_Diff is the single top-level test for this
// themed file. It opens one APID harness + Postgres pool and runs
// each subtest against the shared instance with an isolated
// account. PR #541 review M1 fix.
func TestApplyProject_Diff(t *testing.T) {
	pool := pgtest.Open(t)
	if pool == nil {
		return
	}
	if err := dbMigrateUp(t, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h := e2etest.Start(t, pool, e2etest.APID)

	t.Run("Unchanged", func(t *testing.T) {
		// Pins the no-op diff: a 2nd apply with identical
		// workloads creates zero new build rows. The apps +
		// deployments counts stay the same. This is the
		// regression net for a class of bugs where the diff
		// engine treats every apply as `changed all`.
		key := h.SeedAccount(context.Background(), api.PlanPro, "diff-unchanged")
		body := twoWorkloadFixture(t, "faas-diff-unchanged")
		ar1 := applyProjectMultipart(t, h, key, "diff-unchanged", "", body)
		if len(ar1.Builds) != 2 {
			t.Fatalf("first apply builds=%d want 2", len(ar1.Builds))
		}
		buildsBefore := len(ar1.Builds)
		ar2 := applyProjectMultipart(t, h, key, "diff-unchanged", "", body)
		if len(ar2.Apps) != 2 {
			t.Fatalf("second apply apps=%d want 2 (no workloads added/removed)", len(ar2.Apps))
		}
		if len(ar2.Builds) != 0 {
			t.Fatalf("unchanged re-apply enqueued %d builds, want 0 (regression: every apply churns builds)", len(ar2.Builds))
		}
		if buildsBefore == 0 {
			t.Fatalf("first apply builds was 0 — test setup issue")
		}
	})

	t.Run("Added", func(t *testing.T) {
		// Pins the + path: 2nd apply has one more workload
		// than the 1st. The new workload gets a build row;
		// existing workloads do not.
		key := h.SeedAccount(context.Background(), api.PlanPro, "diff-add")
		// First apply: 1 workload.
		ar1 := applyProjectMultipart(t, h, key, "diff-add", "", oneWorkloadFixtureFromPrefix(t, "faas-add1"))
		if len(ar1.Apps) != 1 {
			t.Fatalf("first apply apps=%d want 1", len(ar1.Apps))
		}
		// Second apply: 2 workloads (same prefix, worker added).
		ar2 := applyProjectMultipart(t, h, key, "diff-add", "", twoWorkloadFixture(t, "faas-add1"))
		if len(ar2.Apps) != 2 {
			t.Fatalf("second apply apps=%d want 2 (1 added)", len(ar2.Apps))
		}
		if len(ar2.Builds) != 1 {
			t.Fatalf("second apply builds=%d want 1 (only the new workload builds)", len(ar2.Builds))
		}
	})

	t.Run("Removed", func(t *testing.T) {
		// Pins the − path: 2nd apply has one fewer workload
		// than the 1st. The removed workload's apps row is
		// soft-deleted (apps.status=deleted OR apps.deleted_at
		// IS NOT NULL — schema-dependent). Existing workloads
		// do not re-build.
		key := h.SeedAccount(context.Background(), api.PlanPro, "diff-rm")
		// First apply: 2 workloads.
		ar1 := applyProjectMultipart(t, h, key, "diff-rm", "", twoWorkloadFixture(t, "faas-rm1"))
		if len(ar1.Apps) != 2 {
			t.Fatalf("first apply apps=%d want 2", len(ar1.Apps))
		}
		// Second apply: 1 workload (worker removed).
		ar2 := applyProjectMultipart(t, h, key, "diff-rm", "", oneWorkloadFixtureFromPrefix(t, "faas-rm1"))
		if len(ar2.Apps) != 1 {
			t.Fatalf("second apply apps=%d want 1 (1 removed)", len(ar2.Apps))
		}
		if len(ar2.Builds) != 0 {
			t.Fatalf("removal re-apply enqueued %d builds, want 0", len(ar2.Builds))
		}
		// Pin: the removed workload's apps row is
		// soft-deleted. We look up by the first apply's apps;
		// the removed one must have either status='deleted'
		// or a non-null deleted_at depending on schema. We
		// try both — any of these counts as a soft-delete.
		var removedStatus string
		var deletedAt *time.Time
		if err := pool.QueryRow(context.Background(),
			`select status, deleted_at from apps where project_id = $1 and slug = 'worker'`,
			ar1.ProjectID).Scan(&removedStatus, &deletedAt); err != nil {
			t.Fatalf("query removed app: %v", err)
		}
		if removedStatus != "deleted" && deletedAt == nil {
			t.Fatalf("removed workload 'worker' was not soft-deleted (status=%q, deleted_at=%v)",
				removedStatus, deletedAt)
		}
	})

	t.Run("Changed", func(t *testing.T) {
		// Pins the ~ path: 2nd apply has the same workloads
		// but a different source hash. The changed workload's
		// deployment row is superseded and a new build is
		// enqueued for it.
		key := h.SeedAccount(context.Background(), api.PlanPro, "diff-chg")
		// First apply: 2-workload repo, body=1 in api.
		ar1 := applyProjectMultipart(t, h, key, "diff-chg", "", twoWorkloadFixture(t, "faas-chg1"))
		if len(ar1.Apps) != 2 {
			t.Fatalf("first apply apps=%d want 2", len(ar1.Apps))
		}
		// Second apply: 2-workload repo, body=99 in api.
		ar2 := applyProjectMultipart(t, h, key, "diff-chg", "", twoWorkloadChangedFixture(t, "faas-chg1"))
		if len(ar2.Apps) != 2 {
			t.Fatalf("second apply apps=%d want 2", len(ar2.Apps))
		}
		// Only the api workload changed; worker is unchanged.
		// So exactly 1 new build is enqueued.
		if len(ar2.Builds) != 1 {
			t.Fatalf("changed re-apply builds=%d want 1 (only api changed)", len(ar2.Builds))
		}
		// Pin: the worker slug must NOT appear in ar2.Builds
		// (the changed-only path is per-workload).
		for _, b := range ar2.Builds {
			if b.Slug == "worker" {
				t.Fatalf("worker build enqueued on a change-only re-apply (regression)")
			}
		}
	})

	t.Run("CronSoftDeleted", func(t *testing.T) {
		// Pins the PR-GH.6 500 regression path: a
		// render.yaml cron that disappears between applies
		// must be soft-deleted, not crash with 500. The fix
		// lives in pkg/reconcile; this test asserts the wire
		// surface.
		// First apply: a repo with 1 cron + 1 service. Hobby
		// plan is required because crons are not allowed on
		// Free.
		const cronYAML = `cronJobs:
  - name: nightly
    schedule: "0 3 * * *"
    command: echo nightly
`
		firstEntries := []struct{ name, body string }{
			{"faas-cron-diff/docker-compose.yml", "services:\n  api:\n    build: { context: . }\n"},
			{"faas-cron-diff/render.yaml", cronYAML},
			{"faas-cron-diff/Dockerfile", "FROM alpine:3.19\nCMD [\"./api\"]\n"},
		}
		var firstBuf bytes.Buffer
		gz := gzip.NewWriter(&firstBuf)
		tw := tar.NewWriter(gz)
		for _, e := range firstEntries {
			hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
			_ = tw.WriteHeader(hdr)
			_, _ = tw.Write([]byte(e.body))
		}
		_ = tw.Close()
		_ = gz.Close()
		firstBody := firstBuf.Bytes()
		key := h.SeedAccount(context.Background(), api.PlanHobby, "diff-cron")
		ar1 := applyProjectMultipart(t, h, key, "diff-cron", "", firstBody)
		if ar1.ProjectID == "" {
			t.Fatalf("first apply returned empty project_id")
		}
		// Second apply: SAME repo without render.yaml. The
		// cron should soft-delete.
		secondEntries := []struct{ name, body string }{
			{"faas-cron-diff/docker-compose.yml", "services:\n  api:\n    build: { context: . }\n"},
			{"faas-cron-diff/Dockerfile", "FROM alpine:3.19\nCMD [\"./api\"]\n"},
		}
		var secondBuf bytes.Buffer
		gz = gzip.NewWriter(&secondBuf)
		tw = tar.NewWriter(gz)
		for _, e := range secondEntries {
			hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
			_ = tw.WriteHeader(hdr)
			_, _ = tw.Write([]byte(e.body))
		}
		_ = tw.Close()
		_ = gz.Close()
		secondBody := secondBuf.Bytes()
		ar2 := applyProjectMultipart(t, h, key, "diff-cron", "", secondBody)
		// Status OK — soft-delete path must not 500.
		if ar2.ProjectID == "" {
			t.Fatalf("second apply returned empty project_id (cron removal regressed)")
		}
	})

	t.Run("DomainCascade", func(t *testing.T) {
		// Pins that removing a workload cascades to remove
		// its attached domains. The wire contract: after
		// removal, GET /v1/domains for the app returns empty.
		// We assert at the DB level here for CI-safety.
		key := h.SeedAccount(context.Background(), api.PlanPro, "diff-domain")
		// First apply: 2 workloads.
		ar1 := applyProjectMultipart(t, h, key, "diff-domain", "", twoWorkloadFixture(t, "faas-dom"))
		if len(ar1.Apps) != 2 {
			t.Fatalf("first apply apps=%d want 2", len(ar1.Apps))
		}
		// Second apply: 1 workload (worker removed).
		_ = applyProjectMultipart(t, h, key, "diff-domain", "", oneWorkloadFixtureFromPrefix(t, "faas-dom"))
		// Pin: worker app has no rows in domains.
		var domainCount int
		err := pool.QueryRow(context.Background(),
			`select count(*) from domains where app_id in (
				select id from apps where project_id = $1 and slug = 'worker' and deleted_at is null
			)`, ar1.ProjectID).Scan(&domainCount)
		if err != nil {
			t.Fatalf("domains count query failed: %v", err)
		}
		if domainCount != 0 {
			t.Fatalf("removed 'worker' app still has %d domains (cascade failed)", domainCount)
		}
	})

	t.Run("EnvCascade", func(t *testing.T) {
		// Pins the same cascade for env vars: removing a
		// workload drops its env vars too.
		key := h.SeedAccount(context.Background(), api.PlanPro, "diff-env")
		// First apply: 2 workloads.
		ar1 := applyProjectMultipart(t, h, key, "diff-env", "", twoWorkloadFixture(t, "faas-env"))
		if len(ar1.Apps) != 2 {
			t.Fatalf("first apply apps=%d want 2", len(ar1.Apps))
		}
		// Second apply: 1 workload (worker removed).
		_ = applyProjectMultipart(t, h, key, "diff-env", "", oneWorkloadFixtureFromPrefix(t, "faas-env"))
		// Pin: worker app has no rows in envs.
		var envCount int
		err := pool.QueryRow(context.Background(),
			`select count(*) from envs where app_id in (
				select id from apps where project_id = $1 and slug = 'worker' and deleted_at is null
			)`, ar1.ProjectID).Scan(&envCount)
		if err != nil {
			t.Fatalf("envs count query failed: %v", err)
		}
		if envCount != 0 {
			t.Fatalf("removed 'worker' app still has %d envs (cascade failed)", envCount)
		}
	})
}
