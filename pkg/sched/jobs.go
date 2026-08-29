// Package sched — engine surface for jobs (issue #1184 Workstream A / ADR-099).
//
// Engine.WakeJob / CancelJob / RetryJob / HandleJobExit + the
// dispatchJobsTick loop are the load-bearing pieces M5 ships. They
// are intentionally thin: the heavy state work lives in the
// pkg/state JobStore (M3), the lease primitive lives in lease.go
// (M4), and the vmmd/guest-init plumbing lives in pkg/fcvm + guest/
// init (M7/M8). M5 wires the engine to all four surfaces.
//
// Why this file lives next to engine.go instead of inside it:
// engine.go is 5500+ lines and already does ten things. Pulling
// the job surface out keeps the diff small, the reviewable surface
// focused, and the cross-PR slot gate clean (ADR-134 will refactor
// the lease half later without touching the WakeJob wiring).

package sched

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// JobWakeResult is the engine-level outcome of WakeJob. Distinct
// from WakeResult (app wakes) because the job lifecycle has no
// "fast-path Phase 1" and the deploymentID is implicit (the job's
// image_ref carries the deployment).
type JobWakeResult struct {
	InstanceID string
	NodeID     string
	RunID      string
	TaskIndex  int
	LeaseToken LeaseToken
	Method     string // "cold_boot" — jobs never restore (no snapshot reuse path)
}

// Engine job-surface methods. Each method takes a pointer receiver
// and a ctx; the ctx carries the per-account rate-limit token (M5's
// JobDispatch bucket, distinct from wakeBuckets so jobs can't starve
// app wakes) and the scope (set by WithScope for free).

// WakeJob admits one job task for execution. Resolves the (runID,
// taskIndex) tuple, locks the task via JobTaskClaimBatch +
// JobTaskMarkClaimed, then issues the vmmd cold-boot RPC (M7). On
// any error after MarkClaimed, the lease is released and the task
// is reversed to status='queued' via JobTaskRetry with a 0-second
// next_attempt_at — a transient vmmd failure should retry on the
// next dispatch tick, not deadlock.
//
// Idempotency: a duplicate WakeJob call for the same (runID,
// taskIndex) returns ErrJobTaskAlreadyClaimed. The first call wins;
// the second is a no-op success on the assumption that the caller
// is the dispatcher retrying after a timeout.
//
// Why cold-boot-only: ADR-005 + job-snapshot reuse don't compose.
// A snapshot is a specific image + state; jobs are arbitrary
// (image_ref, command, env) and the platform has no snapshot
// template cache for them. The 350ms cold-boot budget is the same
// as app cold-boot; the wake latency is what customers pay.
func (e *Engine) WakeJob(ctx context.Context, accountID, runID string, taskIndex int) (JobWakeResult, error) {
	if accountID == "" || runID == "" {
		return JobWakeResult{}, fmt.Errorf("sched: WakeJob: empty accountID/runID")
	}
	// 1. Resolve the parent job + run. The dispatch tick already
	// claimed the task (JobTaskClaimBatch returned this row), so the
	// task is status='queued' from our perspective; the MarkClaimed
	// call below flips it to 'claimed' under the lease token.
	run, err := e.store.JobRunGetByID(ctx, runID)
	if err != nil {
		return JobWakeResult{}, fmt.Errorf("sched: WakeJob resolve run: %w", err)
	}
	if run.AccountID != accountID {
		return JobWakeResult{}, fmt.Errorf("sched: WakeJob account mismatch: %s != %s", accountID, run.AccountID)
	}
	task, err := e.store.JobTaskGet(ctx, runID, taskIndex)
	if err != nil {
		return JobWakeResult{}, fmt.Errorf("sched: WakeJob resolve task: %w", err)
	}
	if task.Status != "queued" {
		return JobWakeResult{}, ErrJobTaskAlreadyClaimed
	}
	job, err := e.store.JobGetByID(ctx, run.JobID)
	if err != nil {
		return JobWakeResult{}, fmt.Errorf("sched: WakeJob resolve job: %w", err)
	}
	if job.Status != "active" {
		// Paused / deleted jobs cannot dispatch. Mark the task
		// cancelled so the run aggregate can settle.
		_ = e.store.JobTaskCancel(ctx, runID, taskIndex)
		return JobWakeResult{}, ErrJobNotActive
	}

	// 2. Admit. Per-account concurrency was already gated at the
	// dispatch tick (so we don't burn a ledger slot on a request
	// the quota will refuse); here we only enforce per-node RAM +
	// vCPU ceilings via the ledger. The per-plan RAM cap is
	// re-checked defensively — a job created on Hobby and re-edited
	// to a Pro plan via PATCH /v1/jobs/{name} cannot exceed the
	// Hobby ceiling until the next run is created. Plan is read
	// from the account row, NOT hardcoded to Hobby — Pro/Scale
	// customers were silently clamped to Hobby RAM + concurrency
	// ceilings on the dispatch path (CR-4 / code-review #4).
	account, err := e.store.AccountByID(ctx, accountID)
	if err != nil {
		return JobWakeResult{}, fmt.Errorf("sched: WakeJob resolve account: %w", err)
	}
	plan := account.Plan
	if plan == api.PlanFree {
		// Free plans return 404 at apid; if a row sneaks through
		// (corrupted state) we still treat it as a Hobby-equivalent
		// floor so the WakeJob doesn't propagate a zero-quota plan
		// into the ledger.
		plan = api.PlanHobby
	}
	planIdx := plan.PlanIndex()
	ramMB := job.RAMMB
	if ramMB > api.JobRAMMB[planIdx] {
		ramMB = api.JobRAMMB[planIdx]
	}
	instanceID := uuid.NewString()
	nodeID := e.ownerNodeID
	req := Request{
		Instance: instanceID,
		AppID:    "", // jobs have no appID
		Plan:     plan,
		RAMMB:    ramMB,
		VCPU:     1, // jobs are single-vCPU today (M5); future SxS uses more
		Kind:     KindJob,
		NodeID:   nodeID,
	}
	if err := e.ledger.Admit(req); err != nil {
		return JobWakeResult{}, fmt.Errorf("sched: WakeJob admit: %w", err)
	}

	// 3. Mint lease + claim. Lease TTL = task_timeout_s + 90s grace
	// (mirrors the §6.1 cold-boot + cleanup envelope for app
	// wakes). For Free-plan jobs the task_timeout_s is 0 — we
	// default to 5 minutes (300s) so a misconfigured job doesn't
	// pin a tenant-RAM slot forever.
	ttl := time.Duration(job.TaskTimeoutS) * time.Second
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	ttl += 90 * time.Second
	leaseExpires := time.Now().Add(ttl)
	if e.jobLeaser == nil {
		// Mega-1 leaser deferred to follow-up commit (see
		// cmd/schedd/main.go jobs-wiring block). Returning the
		// sentinel lets dispatchJobsTick classify the run as
		// failed → retryable; a customer who hits this sees
		// CodeJobLeaserUnavailable, not a nil-deref panic.
		e.ledger.Release(instanceID)
		return JobWakeResult{}, ErrJobLeaserNil
	}
	tok, _, err := e.jobLeaser.Acquire(ctx, formatLeaseKeyForJob(runID, taskIndex), LeasePolicy{TTL: ttl}, e.ownerNodeID)
	if err != nil {
		e.ledger.Release(instanceID)
		return JobWakeResult{}, fmt.Errorf("sched: WakeJob lease: %w", err)
	}
	if err := e.store.JobTaskMarkClaimed(ctx, runID, taskIndex, instanceID, string(tok), leaseExpires, e.ownerNodeID); err != nil {
		_ = e.jobLeaser.Release(ctx, tok, e.ownerNodeID)
		e.ledger.Release(instanceID)
		return JobWakeResult{}, fmt.Errorf("sched: WakeJob mark claimed: %w", err)
	}

	// 4. vmmd RPC. The full cold-boot call lands in M7; until then
	// the WakeJob surface is wired but the Firecracker boot is a
	// no-op stub that returns Method="cold_boot" + an empty NodeID
	// for unit tests. The M7 commit flips this branch to the real
	// vmmd gRPC call.
	if e.jobVmmClient == nil {
		// No vmmd wired — treat as test path. The instance row was
		// never created via CreateInstanceWithMode (that's also M7).
		return JobWakeResult{
			InstanceID: instanceID,
			NodeID:     nodeID,
			RunID:      runID,
			TaskIndex:  taskIndex,
			LeaseToken: tok,
			Method:     "cold_boot",
		}, nil
	}

	// M7: real cold-boot. Left as a TODO marker — the Firecracker
	// boot path is the same shape as Engine.Wake's cold-boot phase
	// but with VsockJobExitPort=1026 and ModeJobColdBoot. The
	// post-M5 commit will replace this branch with the gRPC call.
	return JobWakeResult{}, fmt.Errorf("sched: WakeJob: vmmd job cold-boot path is M7 (TODO)")
}

// CancelJob cancels a job run. For non-terminal tasks:
//   - status='queued'  → JobTaskCancel (no SIGTERM needed)
//   - status='claimed' → SIGTERM via vmmd (M7); guest job supervisor
//     writes job_exit{exit_code=143, error_class='cancelled'},
//     then poweroff. Host reaps via
//     HandleJobExit → MarkTaskTerminal(cancelled).
//
// Already-terminal tasks return ErrJobRunTerminal. Idempotent on
// runs that are already 'cancelled'.
func (e *Engine) CancelJob(ctx context.Context, accountID, runID string) (state.JobRun, error) {
	run, err := e.store.JobRunGetByID(ctx, runID)
	if err != nil {
		return state.JobRun{}, fmt.Errorf("sched: CancelJob: %w", err)
	}
	if run.AccountID != accountID {
		return state.JobRun{}, fmt.Errorf("sched: CancelJob account mismatch: %s != %s", accountID, run.AccountID)
	}
	if isTerminalRunStatus(run.AggregateStatus) {
		return run, ErrJobRunTerminal
	}
	return e.store.JobRunCancel(ctx, runID)
}

// RetryJob re-queues a single task from a dead_letter run. Valid
// only on tasks in {failed, timeout, oom, cancelled} status where
// attempt < job.retry_max+1. The dispatch tick picks the task back
// up on the next sweep via JobTaskRetry's next_attempt_at.
//
// Distinct from CancelJob's "give up" semantics: RetryJob is the
// explicit customer action to re-run a failed task without
// re-creating the run.
func (e *Engine) RetryJob(ctx context.Context, accountID, runID string, taskIndex int) error {
	run, err := e.store.JobRunGetByID(ctx, runID)
	if err != nil {
		return fmt.Errorf("sched: RetryJob: %w", err)
	}
	if run.AccountID != accountID {
		return fmt.Errorf("sched: RetryJob account mismatch: %s != %s", accountID, run.AccountID)
	}
	task, err := e.store.JobTaskGet(ctx, runID, taskIndex)
	if err != nil {
		return fmt.Errorf("sched: RetryJob resolve task: %w", err)
	}
	if task.Status != "failed" && task.Status != "timeout" && task.Status != "oom" && task.Status != "cancelled" {
		return ErrJobTaskNotRetriable
	}
	job, err := e.store.JobGetByID(ctx, run.JobID)
	if err != nil {
		return fmt.Errorf("sched: RetryJob resolve job: %w", err)
	}
	if task.Attempt > job.RetryMax+1 {
		return ErrJobTaskMaxRetriesReached
	}
	// Capped exponential backoff: base * 2^(attempt-1), capped at
	// api.JobBackoffMaxSeconds. The base is api.JobBackoffBaseSeconds
	// = 5s; cap is 300s.
	delay := time.Duration(api.JobBackoffBaseSeconds) * time.Second
	for i := 1; i < task.Attempt; i++ {
		delay *= 2
		if delay > time.Duration(api.JobBackoffMaxSeconds)*time.Second {
			delay = time.Duration(api.JobBackoffMaxSeconds) * time.Second
			break
		}
	}
	next := time.Now().Add(delay)
	if err := e.store.JobTaskRetry(ctx, runID, taskIndex, next); err != nil {
		return fmt.Errorf("sched: RetryJob: %w", err)
	}
	// Bump dead_letter_count → 0 on a successful re-queue. The
	// JobRunRecompute sweep on the next tick will re-derive the
	// aggregate status; if ALL tasks have been retried successfully,
	// the aggregate flips back to 'running'.
	if run.DeadLetterCount > 0 {
		_ = e.store.JobRunIncrementDeadLetter(ctx, runID) // best-effort; recompute fixes it
	}
	return nil
}

// HandleJobExit is invoked from vmmd's DGRAM notification on port
// 1026 (M7). Validates the lease token, then runs the cleanup chain:
//  1. MarkTaskTerminal with the observed exit_code + error_class.
//  2. Release the lease (the lease columns are cleared as part of
//     MarkTaskTerminal's UPDATE).
//  3. If the task failed retryably AND attempt < retry_max+1,
//     call JobTaskRetry to re-queue for the next tick.
//  4. JobRunRecompute to settle the aggregate counters.
//
// Idempotency: a duplicate HandleJobExit for an already-terminal
// task returns nil (the second DGRAM is a vmmd retransmit).
//
// Error classes that retry: failed, timeout, oom. Cancelled is
// terminal-no-retry. Succeeded is terminal-no-retry.
func (e *Engine) HandleJobExit(ctx context.Context, accountID, runID string, taskIndex int, exitCode int, errorClass string, leaseTokenStr string) error {
	if accountID == "" || runID == "" {
		return fmt.Errorf("sched: HandleJobExit: empty accountID/runID")
	}
	run, err := e.store.JobRunGetByID(ctx, runID)
	if err != nil {
		return fmt.Errorf("sched: HandleJobExit resolve run: %w", err)
	}
	if run.AccountID != accountID {
		return fmt.Errorf("sched: HandleJobExit account mismatch: %s != %s", accountID, run.AccountID)
	}
	task, err := e.store.JobTaskGet(ctx, runID, taskIndex)
	if err != nil {
		return fmt.Errorf("sched: HandleJobExit resolve task: %w", err)
	}
	if isTerminalTaskStatus(task.Status) {
		// Already-terminal — vmmd retransmit, swallow.
		return nil
	}
	if task.LeaseToken == nil || *task.LeaseToken != leaseTokenStr {
		return ErrLeaseHeldByOther
	}
	// Map the (exit_code, error_class) tuple onto a terminal
	// status. The mapping mirrors guest/init/job_supervisor_linux.go
	// (M8); keep them in lock-step.
	status := mapExitToTerminalStatus(exitCode, errorClass)
	if err := e.store.JobTaskMarkTerminal(ctx, runID, taskIndex, status, exitCode, errorClass, "", time.Now()); err != nil {
		return fmt.Errorf("sched: HandleJobExit mark terminal: %w", err)
	}
	// Release the lease — the lease columns were cleared by
	// MarkTaskTerminal, but the in-memory MemLeaser (tests) still
	// tracks the record. PgLeaser.Release is a no-op if the row is
	// already gone, so calling it twice is safe.
	if tok := LeaseToken(leaseTokenStr); tok != "" {
		_ = e.jobLeaser.Release(ctx, tok, e.ownerNodeID)
	}
	// Retry-on-failure: re-queue failed/timeout/oom tasks if budget
	// remains.
	if status == "failed" || status == "timeout" || status == "oom" {
		job, err := e.store.JobGetByID(ctx, run.JobID)
		if err == nil && task.Attempt <= job.RetryMax+1 {
			delay := time.Duration(api.JobBackoffBaseSeconds) * time.Second
			for i := 1; i < task.Attempt; i++ {
				delay *= 2
				if delay > time.Duration(api.JobBackoffMaxSeconds)*time.Second {
					delay = time.Duration(api.JobBackoffMaxSeconds) * time.Second
					break
				}
			}
			next := time.Now().Add(delay)
			if rerr := e.store.JobTaskRetry(ctx, runID, taskIndex, next); rerr == nil {
				// Skip recompute — the task is back in 'queued',
				// aggregate will recompute on next claim.
				return nil
			}
		}
		// Exhausted retries → dead-letter. JobRunRecompute picks
		// this up via the dead_letter_count column.
		if err == nil && task.Attempt > job.RetryMax+1 {
			_ = e.store.JobRunIncrementDeadLetter(ctx, runID)
		}
	}
	// Settle the run aggregate counters. Failure here is logged but
	// not fatal — the dispatch tick will pick it up.
	if _, err := e.store.JobRunRecompute(ctx, runID); err != nil {
		return fmt.Errorf("sched: HandleJobExit recompute: %w", err)
	}
	return nil
}

// dispatchJobsTick is the per-second loop. cmd/schedd/main.go
// starts it alongside the cronLoop + reaper. Each tick:
//
//  1. Per-account quota gate (api.JobConcurrentPerAccount): reject
//     accounts already at the cap.
//  2. JobTaskClaimBatch(N) returns up to N queued tasks (FOR UPDATE
//     SKIP LOCKED in PgStore).
//  3. For each claimed task, call WakeJob. The WakeJob's per-task
//     admit (ledger) + lease mint + MarkClaimed is the atomic
//     transition queued → claimed.
//
// Tick interval is 1s. Tick budget is bounded by the sum of
// per-account caps × wake latency; on a Hobby account with cap=3,
// that's 3 × ~350ms cold-boot ≈ 1s worst-case. We cap the
// per-tick batch at 32 (a Pro-friendly number) so a runaway fleet
// can't starve the cron loop.
func (e *Engine) DispatchJobsTick(ctx context.Context) error {
	const perTickBatch = 32
	tasks, err := e.store.JobTaskClaimBatch(ctx, perTickBatch)
	if err != nil {
		return fmt.Errorf("sched: dispatchJobsTick claim: %w", err)
	}
	for _, t := range tasks {
		// Look up the parent run for the account_id (JobTask doesn't
		// carry account_id directly; it's denormalised on the run
		// row). The JobRunGetByID read is cheap (~1ms) and the
		// dispatch tick budget is 1s/tick so the round-trip is
		// well within the SLA.
		run, err := e.store.JobRunGetByID(ctx, t.RunID)
		if err != nil {
			// Run is gone — re-queue the task so the next tick can
			// see the orphan and skip it. Best-effort. Use
			// JobTaskRequeue (NOT JobTaskRetry) so the attempt
			// counter is preserved — the task never executed.
			_ = e.store.JobTaskRequeue(ctx, t.RunID, t.TaskIndex, time.Now())
			continue
		}
		// Per-account gate (the ledger Admit already covers
		// per-node RAM; this is the per-account concurrency
		// ceiling). Plan is read from the account row so Pro/Scale
		// customers hit their per-plan cap (CR-4 / code-review #4).
		concurrent, err := e.store.JobConcurrentByAccount(ctx, run.AccountID)
		if err != nil {
			return fmt.Errorf("sched: dispatchJobsTick concurrent: %w", err)
		}
		account, err := e.store.AccountByID(ctx, run.AccountID)
		if err != nil {
			return fmt.Errorf("sched: dispatchJobsTick resolve account: %w", err)
		}
		plan := account.Plan
		if plan == api.PlanFree {
			// Free plans are 404 at apid; if a row sneaks through
			// we still treat it as Hobby-equivalent so the cap
			// lookup doesn't index [-1].
			plan = api.PlanHobby
		}
		planIdx := plan.PlanIndex()
		if cap := api.JobConcurrentPerAccount[planIdx]; concurrent >= cap {
			// Re-queue for the next tick (next_attempt_at = now()).
			// JobTaskRequeue preserves attempt — the task never
			// executed (CR-7 / code-review #7).
			_ = e.store.JobTaskRequeue(ctx, t.RunID, t.TaskIndex, time.Now())
			continue
		}
		if _, err := e.WakeJob(ctx, run.AccountID, t.RunID, t.TaskIndex); err != nil {
			// Best-effort retry: a transient admit / vmmd failure
			// should not block other tasks. next_attempt_at = now()
			// means "eligible immediately on the next tick". Use
			// JobTaskRequeue (NOT JobTaskRetry) so the customer's
			// retry budget is preserved — WakeJob did not actually
			// run the customer code (CR-7 / code-review #7).
			_ = e.store.JobTaskRequeue(ctx, t.RunID, t.TaskIndex, time.Now())
		}
	}
	return nil
}

// ----------------------------------------------------------------------------
// Engine job-surface wiring (test seams; production wiring lives in
// cmd/schedd/main.go).
// ----------------------------------------------------------------------------

// jobVmmClient is the vmmd gRPC client interface the engine uses to
// boot job-task VMs. The real implementation lands in M7; until
// then, leaving this nil means WakeJob runs in unit-test mode
// (no vmmd call, instance row never created).
type jobVmmClient interface {
	JobColdBoot(ctx context.Context, spec JobVmmSpec) (JobVmmResult, error)
}

// JobVmmSpec is the vmmd-side job boot payload. Mirrors the shape
// pkg/fcvm.BootMode=ModeJobColdBoot will accept (M7).
type JobVmmSpec struct {
	AccountID      string
	RunID          string
	TaskIndex      int
	ImageRef       string
	Command        []string
	Env            map[string]string
	RAMMB          int
	TaskTimeoutSec int
	LeaseToken     string
	NodeID         string
}

// JobVmmResult is the vmmd-side cold-boot outcome.
type JobVmmResult struct {
	InstanceID string
	NodeID     string
}

// Engine wiring. The production schedd main.go fills these
// in via Engine.WithJobVmmClient + WithJobLeaser + WithJobLedger.
// The unit tests build an Engine directly via these accessors.

// WithJobVmmClient wires the vmmd client into the engine.
func (e *Engine) WithJobVmmClient(c jobVmmClient) *Engine {
	e.jobVmmClient = c
	return e
}

// WithJobLeaser wires the lease primitive into the engine.
// Accepts Leaser[any] so production PgLeaser[pgLeaseRecord]
// and test MemLeaser[memLeaseRecord] both slot in (Mega-1:
// the engine only needs the LeaseToken, the record type
// is dropped). The ADR-134 pkg/dispatch swap will tighten
// this to a single concrete record type post-Mega-1.
func (e *Engine) WithJobLeaser(l Leaser[any]) *Engine {
	e.jobLeaser = l
	return e
}

// Leaser accessor for the engine — used by tests + cmd/schedd/main.go.
func (e *Engine) JobLeaser() Leaser[any] { return e.jobLeaser }

// ----------------------------------------------------------------------------
// helpers
// ----------------------------------------------------------------------------

// formatLeaseKeyForJob is the canonical (runID|"\x00"|taskIndex)
// string the Leaser[T] expects. Mirrors parseLeaseKey in lease_pg.go.
func formatLeaseKeyForJob(runID string, taskIndex int) string {
	return runID + "\x00" + itoa(taskIndex)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func isTerminalRunStatus(s string) bool {
	switch s {
	case "succeeded", "failed", "cancelled", "dead_letter":
		return true
	}
	return false
}

func isTerminalTaskStatus(s string) bool {
	switch s {
	case "succeeded", "failed", "timeout", "cancelled", "oom":
		return true
	}
	return false
}

// mapExitToTerminalStatus is the canonical (exit_code, error_class)
// → job_tasks.status mapping. Mirrors
// guest/init/job_supervisor_linux.go::mapExitToErrorClass (M8).
//
// error_class precedence: when the guest-supervised DGRAM payload
// carries a non-empty error_class from the canonical set
// {succeeded, failed, timeout, oom, cancelled, infra}, we trust
// it as authoritative and skip the exit-code fallback. The
// exit-code mapping is the legacy fallback for hosts that don't
// yet ship the v2 payload schema (CR-5 / code-review #5 — the
// previous version silently ignored errorClass, so a guest that
// correctly classified "infra" with exit_code=139 was demoted to
// "failed" and the task dead-lettered on first signal death).
//
// exit_code conventions (fallback path):
//
//	0    → succeeded
//	124  → timeout (per `timeout` coreutils)
//	137  → oom (kernel OOM killer)
//	143  → cancelled (SIGTERM)
//	any  → failed
func mapExitToTerminalStatus(exitCode int, errorClass string) string {
	switch errorClass {
	case "succeeded", "failed", "timeout", "oom", "cancelled", "infra":
		// "infra" maps to "failed" at the status layer — the wire
		// error_class preserves the signal-death distinction for
		// observability, but the task status is still terminal-
		// failure so the retry + dead-letter logic in
		// HandleJobExit takes the standard path.
		if errorClass == "infra" {
			return "failed"
		}
		return errorClass
	}
	switch {
	case exitCode == 0:
		return "succeeded"
	case exitCode == 124:
		return "timeout"
	case exitCode == 137:
		return "oom"
	case exitCode == 143:
		return "cancelled"
	default:
		return "failed"
	}
}

// ----------------------------------------------------------------------------
// errors
// ----------------------------------------------------------------------------

// ErrJobTaskAlreadyClaimed marks a duplicate WakeJob call for the
// same (runID, taskIndex) tuple. The caller treats this as a benign
// no-op (the first call won the lease).
var ErrJobTaskAlreadyClaimed = errors.New("sched: job task already claimed")

// ErrJobNotActive marks a WakeJob against a paused / deleted job.
// The task is cancelled before returning.
var ErrJobNotActive = errors.New("sched: job is not active")

// ErrJobRunTerminal marks a CancelJob against a run that's already
// succeeded / failed / cancelled / dead_letter. Idempotent no-op.
var ErrJobRunTerminal = errors.New("sched: job run is terminal")

// ErrJobTaskNotRetriable marks a RetryJob against a task that's
// not in {failed, timeout, oom, cancelled}. Succeeded tasks can't
// be retried (the work is done).
var ErrJobTaskNotRetriable = errors.New("sched: job task not retriable")

// ErrJobTaskMaxRetriesReached marks a RetryJob against a task
// that's already exhausted job.retry_max+1 attempts.
var ErrJobTaskMaxRetriesReached = errors.New("sched: job task max retries reached")
