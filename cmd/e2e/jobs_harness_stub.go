//go:build metal

// jobs_harness_stub.go — Metal-tagged job-helper stubs. The
// real implementations live alongside the existing
// e2etest.Harness and are added in a follow-up commit once the
// metal harness extension lands. Until then these compile-shim
// methods let cmd/e2e/jobs_metal_test.go compile under
// `go test -tags metal` so reviewers can audit the wire shape
// without needing KVM. The stubs panic with a clear TODO if
// any helper is actually invoked (i.e. on a real
// `make metal-lima` run) — the test will fail loud, not silent.
//
// TODO follow-up: implement MetalJobHarness against the existing
// e2etest.Harness. Reference deploy_wake_metal_test.go for the
// boot sequence.

package e2e

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/e2etest"
	"github.com/onebox-faas/faas/pkg/state"
)

// MetalJobHarness is the M14 metal helper struct. The real
// implementation wraps e2etest.Harness + a per-job fake-image
// seeder + the cmd/apid client + a poll-for-task-status loop.
type MetalJobHarness struct {
	t *testing.T
	*e2etest.Harness
}

// Close tears down the harness (kills subprocesses, removes the
// temp dir). Real impl delegates to e2etest.Harness.Stop.
func (h *MetalJobHarness) Close() {
	if h == nil || h.Harness == nil {
		return
	}
	h.Harness.Stop()
}

// newMetalHarness boots apid + schedd + vmmd + firecracker + the
// fake OCI registry, then returns the harness with a pgtest
// schema ready to receive jobs.
func newMetalHarness(t *testing.T) *MetalJobHarness {
	t.Fatal("TODO follow-up: implement MetalJobHarness against e2etest.Harness; " +
		"see cmd/e2e/jobs_metal_test.go for the test contract")
	return nil
}

// MustSeedFakeImage seeds the in-process fake OCI registry with
// one image at the given ref. The image's entrypoint is the
// jobs_metal_test.go workload (varies per test via image tag).
func (h *MetalJobHarness) MustSeedFakeImage(t *testing.T, imageRef string) {
	t.Fatal("TODO: wire fakeregistry.Upload with the workload image")
}

// MustCreateJob POSTs /v1/jobs and returns the persisted JobResponse.
// On non-2xx the helper fails the test loud.
func (h *MetalJobHarness) MustCreateJob(t *testing.T, name, imageRef string, command []string, ramMB int) *api.JobResponse {
	t.Fatal("TODO: harness.CreateJob(name, imageRef, command, ramMB)")
	return nil
}

// MustUpdateJob PATCHes /v1/jobs/{name} with the mutator, returns
// the post-update JobResponse.
func (h *MetalJobHarness) MustUpdateJob(t *testing.T, job *api.JobResponse, mut func(*api.UpdateJobRequest)) *api.JobResponse {
	t.Fatal("TODO: harness.UpdateJob(job, mut)")
	return nil
}

// MustDispatchRun POSTs /v1/jobs/{name}/runs and returns the
// freshly-created JobRunResponse with status=queued.
func (h *MetalJobHarness) MustDispatchRun(t *testing.T, job *api.JobResponse, tasks int) *api.JobRunResponse {
	t.Fatal("TODO: harness.DispatchRun(job, tasks)")
	return nil
}

// MustWaitRunTerminal polls GET /v1/jobs/{name}/runs/{id} until
// run.aggregate_status equals want, fails on timeout.
func (h *MetalJobHarness) MustWaitRunTerminal(t *testing.T, run *api.JobRunResponse, want string, timeout time.Duration) {
	t.Fatal("TODO: poll loop with backoff")
}

// MustWaitTaskStatus polls until task(taskIndex).status == want.
func (h *MetalJobHarness) MustWaitTaskStatus(t *testing.T, run *api.JobRunResponse, taskIndex int, want string, timeout time.Duration) {
	t.Fatal("TODO: poll loop with backoff")
}

// MustWaitTaskAttempt polls until task(taskIndex).attempt == wantAttempt
// AND status == wantStatus.
func (h *MetalJobHarness) MustWaitTaskAttempt(t *testing.T, run *api.JobRunResponse, taskIndex, wantAttempt int, wantStatus string, timeout time.Duration) {
	t.Fatal("TODO: poll loop with backoff")
}

// MustCancelRun POSTs /v1/jobs/{name}/runs/{id}/cancel.
func (h *MetalJobHarness) MustCancelRun(t *testing.T, run *api.JobRunResponse) {
	t.Fatal("TODO: cancelRun(run)")
}

// MustAssertTaskExitCodes verifies all tasks share the same
// expected exit code (0 for happy-path).
func (h *MetalJobHarness) MustAssertTaskExitCodes(t *testing.T, run *api.JobRunResponse, want int) {
	t.Fatal("TODO: assertTaskExitCodes")
}

// MustAssertTaskExitCode verifies a single task's exit_code.
func (h *MetalJobHarness) MustAssertTaskExitCode(t *testing.T, run *api.JobRunResponse, taskIndex, want int) {
	t.Fatal("TODO: assertTaskExitCode")
}

// MustAssertRunDeadLetter verifies run.dead_letter_count == want.
func (h *MetalJobHarness) MustAssertRunDeadLetter(t *testing.T, run *api.JobRunResponse, want int) {
	t.Fatal("TODO: assertRunDeadLetter")
}

// MustAssertNoInstancesForRun verifies the soft-delete-equivalent
// invariant for cancelled-queued runs: zero rows in `instances`
// with kind='job_task' AND instance_id IS NULL.
func (h *MetalJobHarness) MustAssertNoInstancesForRun(t *testing.T, run *api.JobRunResponse) {
	t.Fatal("TODO: query instances WHERE kind='job_task' AND (run_id=$1 OR id=$1)")
}

// MustAssertUsageDailyRows checks the §4.7 metering contract.
func (h *MetalJobHarness) MustAssertUsageDailyRows(t *testing.T, accountID string, jobID string, ramMB int, wantDur, tolerance time.Duration) {
	t.Fatal("TODO: SELECT * FROM usage_daily WHERE meter_kind='job' AND job_id=$1")
}

// MustKillSchedd sends SIGKILL to the schedd subprocess; used by
// the node-loss test.
func (h *MetalJobHarness) MustKillSchedd(t *testing.T) {
	t.Fatal("TODO: harness.scheddCmd.Process.Kill()")
}

// MustRestartSchedd re-launches schedd after MustKillSchedd.
func (h *MetalJobHarness) MustRestartSchedd(t *testing.T) {
	t.Fatal("TODO: harness.bootSchedd()")
}

// MustSetEnv sets a daemon env var (restored on Close).
func (h *MetalJobHarness) MustSetEnv(t *testing.T, key, value string) {
	t.Fatal("TODO: harness.setEnv(key, value)")
}

// MustCreateFreeAccount provisions a Free-plan account + API key
// against the live apid.
func (h *MetalJobHarness) MustCreateFreeAccount(t *testing.T) *state.Account {
	t.Fatal("TODO: store.CreateAccount(ctx, email, PlanFree)")
	return nil
}

// MustCreateHobbyAccount provisions a Hobby-plan account + API key.
func (h *MetalJobHarness) MustCreateHobbyAccount(t *testing.T) *state.Account {
	t.Fatal("TODO: store.CreateAccount(ctx, email, PlanHobby)")
	return nil
}

// MustCreateJobAsAccount lets the test create jobs under a
// specific account context (Free + Hobby quota tests).
func (h *MetalJobHarness) MustCreateJobAsAccount(t *testing.T, acct *state.Account, name, imageRef string, command []string, ramMB int) *api.JobResponse {
	t.Fatal("TODO: CreateJobAsAccount")
	return nil
}

// MustPostAsAccount issues an authenticated POST as a given account.
func (h *MetalJobHarness) MustPostAsAccount(t *testing.T, acct *state.Account, path string, body any) *httptest.ResponseRecorder {
	t.Fatal("TODO: harness.do(acct, POST, path, body)")
	return nil
}
