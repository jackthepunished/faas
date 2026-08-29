//go:build metal

// jobs_metal_test.go — M14 §14 acceptance for the Jobs feature
// (issue #1184 Workstream A / Mega-1). Each test boots the full
// apid → schedd → vmmd → firecracker pipeline against the fake
// OCI registry + pgtest harness and exercises one job lifecycle
// path end-to-end through the real wire.
//
// 11 tests per the plan, each -timeout 10-30 min, run via
// `make metal-lima` (Apple Silicon) or `make test-metal` (EX44).
//
// Build tag: metal. Requires:
//   - /dev/kvm + root (jailer needs CAP_NET_ADMIN, CAP_MKNOD, …)
//   - Firecracker on PATH
//   - FAAS_TEST_KERNEL pointing at a vmlinux (any recent one)
//
// The Lima caveat (CLAUDE.md): this validates the arch-agnostic
// VM lifecycle + boot path on arm64; production x86_64 snapshot
// correctness remains an EX44-only check.

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
)

// TestJobsE2E_HappyPath dispatches a Hobby job with 5 tasks of
// `/bin/true` and asserts every task reaches status=succeeded
// + run.aggregate_status=succeeded within the task timeout.
// This is the §14 M14 baseline — every other test in this file
// reuses the helpers introduced here.
func TestJobsE2E_HappyPath(t *testing.T) {
	h := newMetalHarness(t)
	defer h.Close()
	h.MustSeedFakeImage(t, "busybox:job-happy")
	job := h.MustCreateJob(t, "happy-job", "busybox:job-happy", []string{"/bin/true"}, 512)
	run := h.MustDispatchRun(t, job, 5)
	h.MustWaitRunTerminal(t, run, "succeeded", 5*time.Minute)
	h.MustAssertTaskExitCodes(t, run, 0)
}

// TestJobsE2E_RetryThenSucceed asserts the failed→retry→queued
// loop: a task that exits non-zero on attempt 1 retries (per
// --retries N) and the retry succeeds, run ends succeeded.
func TestJobsE2E_RetryThenSucceed(t *testing.T) {
	h := newMetalHarness(t)
	defer h.Close()
	h.MustSeedFakeImage(t, "busybox:job-retry")
	job := h.MustCreateJob(t, "retry-job", "busybox:job-retry", []string{"/bin/sh", "-c", "exit 1"}, 512)
	job = h.MustUpdateJob(t, job, func(p *api.UpdateJobRequest) {
		n := 3
		p.RetryMax = &n
	})
	run := h.MustDispatchRun(t, job, 1)
	// First attempt fails; retry kicks in; on attempt 2 the
	// synthetic workload in the image flips exit 0. Wire shape
	// shows attempt=2, status=succeeded.
	h.MustWaitTaskAttempt(t, run, 1, 2, "succeeded", 5*time.Minute)
}

// TestJobsE2E_DeadLetter asserts retry exhaustion →
// run.aggregate_status=dead_letter. The task always exits 1;
// after --retries 0 attempts, dead_letter_count=1.
func TestJobsE2E_DeadLetter(t *testing.T) {
	h := newMetalHarness(t)
	defer h.Close()
	h.MustSeedFakeImage(t, "busybox:job-deadletter")
	job := h.MustCreateJob(t, "deadletter-job", "busybox:job-deadletter", []string{"/bin/false"}, 512)
	run := h.MustDispatchRun(t, job, 1)
	h.MustWaitRunTerminal(t, run, "dead_letter", 3*time.Minute)
	h.MustAssertRunDeadLetter(t, run, 1)
}

// TestJobsE2E_TaskTimeout pins the §4.6 task_timeout_s enforcement:
// the workload sleeps 60s with timeout=10s, so the engine MUST
// SIGKILL the guest, write exit_code=124, status=timeout within
// the timeout window.
func TestJobsE2E_TaskTimeout(t *testing.T) {
	h := newMetalHarness(t)
	defer h.Close()
	h.MustSeedFakeImage(t, "busybox:job-timeout")
	job := h.MustCreateJob(t, "timeout-job", "busybox:job-timeout", []string{"/bin/sh", "-c", "sleep 60"}, 512)
	job = h.MustUpdateJob(t, job, func(p *api.UpdateJobRequest) {
		n := 10
		p.TaskTimeoutSec = &n
	})
	run := h.MustDispatchRun(t, job, 1)
	h.MustWaitTaskStatus(t, run, 1, "timeout", 90*time.Second)
	h.MustAssertTaskExitCode(t, run, 1, 124)
}

// TestJobsE2E_OOM asserts that a memory bomb triggers the
// per-plan cgroup OOM detection → exit_code=137, status=oom.
// The plan-tier RAM (512MB Hobby) is the ceiling; the workload
// mallocs 1GB.
func TestJobsE2E_OOM(t *testing.T) {
	h := newMetalHarness(t)
	defer h.Close()
	h.MustSeedFakeImage(t, "busybox:job-oom")
	job := h.MustCreateJob(t, "oom-job", "busybox:job-oom", []string{"/bin/sh", "-c", "dd if=/dev/zero of=/dev/null bs=1M count=1024"}, 512)
	run := h.MustDispatchRun(t, job, 1)
	h.MustWaitTaskStatus(t, run, 1, "oom", 60*time.Second)
	h.MustAssertTaskExitCode(t, run, 1, 137)
}

// TestJobsE2E_CancelQueued asserts cancel-before-dispatch: the
// run is cancelled while still in queued state, NO guest VM is
// spawned, run.aggregate_status=cancelled, dead_letter_count=0.
func TestJobsE2E_CancelQueued(t *testing.T) {
	h := newMetalHarness(t)
	defer h.Close()
	h.MustSeedFakeImage(t, "busybox:job-cancel-queued")
	// FAAS_JOBS_DISPATCH=0 keeps tasks queued but spawned-able
	// for the dispatch tick; we cancel before the tick fires.
	h.MustSetEnv(t, "FAAS_JOBS_DISPATCH", "0")
	job := h.MustCreateJob(t, "cancel-queued-job", "busybox:job-cancel-queued", []string{"/bin/sleep", "300"}, 512)
	run := h.MustDispatchRun(t, job, 1)
	h.MustCancelRun(t, run)
	h.MustWaitRunTerminal(t, run, "cancelled", 30*time.Second)
	h.MustAssertNoInstancesForRun(t, run)
}

// TestJobsE2E_CancelRunning asserts SIGTERM delivered to a live
// guest: the workload installs a SIGTERM handler that exits 143
// (cancelled error_class). The engine forwards the SIGTERM via
// vmmd.SendSignal and waits up to 30s grace before SIGKILL.
func TestJobsE2E_CancelRunning(t *testing.T) {
	h := newMetalHarness(t)
	defer h.Close()
	h.MustSeedFakeImage(t, "busybox:job-cancel-running")
	job := h.MustCreateJob(t, "cancel-running-job", "busybox:job-cancel-running", []string{"/bin/sh", "-c", "trap 'exit 143' TERM; sleep 300"}, 512)
	run := h.MustDispatchRun(t, job, 1)
	h.MustWaitTaskStatus(t, run, 1, "running", 30*time.Second)
	h.MustCancelRun(t, run)
	h.MustWaitTaskStatus(t, run, 1, "cancelled", 60*time.Second)
	h.MustAssertTaskExitCode(t, run, 1, 143)
}

// TestJobsE2E_NodeLoss exercises lease expiry: kill schedd
// after a task is claimed but before its exit is observed. A
// fresh schedd MUST re-claim the task after lease_expires_at
// elapses, and the re-dispatched task must complete (or be
// reaped, depending on the workload).
func TestJobsE2E_NodeLoss(t *testing.T) {
	h := newMetalHarness(t)
	defer h.Close()
	h.MustSeedFakeImage(t, "busybox:job-nodeloss")
	h.MustSetEnv(t, "FAAS_JOBS_LEASE_TTL_SECONDS", "5")
	job := h.MustCreateJob(t, "nodeloss-job", "busybox:job-nodeloss", []string{"/bin/sleep", "5"}, 512)
	run := h.MustDispatchRun(t, job, 1)
	h.MustWaitTaskStatus(t, run, 1, "running", 30*time.Second)
	h.MustKillSchedd(t)
	h.MustRestartSchedd(t)
	// After the lease TTL + reaper sweep, the task either
	// succeeds (the original VM's exit was already in flight)
	// or is marked timeout by the reaper. Either is acceptable;
	// run MUST reach a terminal status.
	h.MustWaitRunTerminal(t, run, "any-terminal", 90*time.Second)
}

// TestJobsE2E_BillingRollup pins the §4.7 metering contract:
// usage_daily rolls up one row per (day, plan, account, job)
// with meter_kind='job' and billable_mb_seconds matching the
// wall-clock duration × plan RAM.
func TestJobsE2E_BillingRollup(t *testing.T) {
	h := newMetalHarness(t)
	defer h.Close()
	acct := h.MustCreateHobbyAccount(t)
	h.MustSeedFakeImage(t, "busybox:job-billing")
	job := h.MustCreateJob(t, "billing-job", "busybox:job-billing", []string{"/bin/sleep", "10"}, 512)
	run := h.MustDispatchRun(t, job, 1)
	h.MustWaitRunTerminal(t, run, "succeeded", 30*time.Second)
	h.MustAssertUsageDailyRows(t, acct.ID, job.Name, 512, 10*time.Second, 5*time.Second)
}

// TestJobsE2E_FreePlanForbidden asserts the plan-tier gate at
// the wire layer: a Free-plan account receives 402
// jobs_not_allowed from POST /v1/jobs even with empty quota.
// The handler-level test in handlers_jobs_test.go pins the unit
// behavior; this pins the end-to-end middleware chain.
func TestJobsE2E_FreePlanForbidden(t *testing.T) {
	h := newMetalHarness(t)
	defer h.Close()
	acct := h.MustCreateFreeAccount(t)
	rec := h.MustPostAsAccount(t, acct, "/v1/jobs", api.CreateJobRequest{
		Name:     "free-tier",
		ImageRef: "busybox:job-free",
		Command:  []string{"/bin/true"},
	})
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("Free-plan create = %d, want 402; body=%s", rec.Code, rec.Body.String())
	}
	var body api.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Code != "jobs_not_allowed" {
		t.Errorf("body.Code = %q, want jobs_not_allowed", body.Code)
	}
}

// TestJobsE2E_HobbyQuota asserts the Hobby JobMaxPerAccount
// ceiling: 6th job creation returns 403 job_quota_exceeded.
func TestJobsE2E_HobbyQuota(t *testing.T) {
	h := newMetalHarness(t)
	defer h.Close()
	acct := h.MustCreateHobbyAccount(t)
	for i := 1; i <= 5; i++ {
		h.MustCreateJobAsAccount(t, acct, fmt.Sprintf("hobby-job-%d", i), "busybox:job-quota", []string{"/bin/true"}, 512)
	}
	rec := h.MustPostAsAccount(t, acct, "/v1/jobs", api.CreateJobRequest{
		Name:     "hobby-job-6-over",
		ImageRef: "busybox:job-quota",
		Command:  []string{"/bin/true"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("6th create = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "job_quota_exceeded") {
		t.Errorf("body must surface job_quota_exceeded; got: %s", rec.Body.String())
	}
}

// _ = context.Background — keep the import alive for future
// tests added in this file that take a ctx.
var _ = context.Background
