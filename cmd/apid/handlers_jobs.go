// Package main — apid job-task handlers (issue #1184 Workstream A /
// ADR-099).
//
// Two-step IDOR-safe lookup pattern mirrors the cron handlers
// (handlers_ext.go::getCron at line 2509): resolve the job, verify
// account_id matches the authenticated account, then return. Both
// failure branches emit identical 404s so a probe cannot distinguish
// "missing" from "cross-account" — same security shape as the cron
// family. Routes are wired in cmd/apid/server.go alongside the cron
// / trigger / app families.
//
// Plan-tier gate (Free → 402 CodeJobsNotAllowed) lives in
// createJob/updateJob/createJobRun (write handlers); read endpoints
// return empty lists for Free accounts (the gate is on write, not
// read — same shape as the cron family's "Free has 0 cron:read but
// still 402 on cron:write" pattern).

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// jobResponse projects state.Job → api.JobResponse. Stable across
// all read endpoints so the SDK can decode with a single type. The
// handler family (listJobs / getJob / write responses) all funnel
// through this helper so a future column addition only needs to
// touch one projection site.
//
// env_overrides is the open-vocabulary jsonb map; decoded
// lazily here so a malformed row surfaces as an empty map (NOT a
// handler error — the handler treats a bad jsonb as a corruption
// signal that the operator CLI surfaces, not the customer-facing
// API).
func jobResponse(j state.Job) api.JobResponse {
	resp := api.JobResponse{
		ID:             j.ID,
		AccountID:      j.AccountID,
		Name:           j.Name,
		Kind:           j.Kind,
		ImageRef:       j.ImageRef,
		Command:        j.Command,
		RAMMB:          j.RAMMB,
		TaskTimeoutSec: j.TaskTimeoutS,
		MaxParallelism: j.MaxParallelism,
		RetryMax:       j.RetryMax,
		Status:         j.Status,
		CreatedAt:      j.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:      j.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if len(j.EnvOverrides) > 0 {
		var env map[string]string
		if err := json.Unmarshal(j.EnvOverrides, &env); err == nil {
			resp.EnvOverrides = env
		}
	}
	if resp.Command == nil {
		resp.Command = []string{}
	}
	return resp
}

// jobRunResponse projects state.JobRun → api.JobRunResponse.
// Aggregate counters come straight from the row (JobRunRecompute
// updates them after every terminal task transition); the wire
// shape stays stable across run lifecycle stages.
//
// RetryMax / TaskTimeoutSec are *int on the row (nil = inherit
// from jobs.*); we dereference here so the wire carries the
// effective integer value (the inheritance was already resolved
// at run-create time per migrations/00255 + 00528).
func jobRunResponse(r state.JobRun) api.JobRunResponse {
	resp := api.JobRunResponse{
		ID:              r.ID,
		JobID:           r.JobID,
		AccountID:       r.AccountID,
		TriggerKind:     r.TriggerKind,
		Tasks:           r.Tasks,
		Parallelism:     r.Parallelism,
		AggregateStatus: r.AggregateStatus,
		TasksSucceeded:  r.TasksSucceeded,
		TasksFailed:     r.TasksFailed,
		TasksCancelled:  r.TasksCancelled,
		TasksRunning:    r.TasksRunning,
		DeadLetterCount: r.DeadLetterCount,
		CreatedAt:       r.CreatedAt.UTC().Format(time.RFC3339),
	}
	if r.RetryMax != nil {
		resp.RetryMax = *r.RetryMax
	}
	if r.TaskTimeoutS != nil {
		resp.TaskTimeoutSec = *r.TaskTimeoutS
	}
	if r.EnvOverrides != nil {
		var env map[string]string
		if err := json.Unmarshal(r.EnvOverrides, &env); err == nil {
			resp.EnvOverrides = env
		}
	}
	if r.StartedAt != nil && !r.StartedAt.IsZero() {
		resp.StartedAt = r.StartedAt.UTC().Format(time.RFC3339)
	}
	if r.FinishedAt != nil && !r.FinishedAt.IsZero() {
		resp.FinishedAt = r.FinishedAt.UTC().Format(time.RFC3339)
	}
	return resp
}

// jobTaskResponse projects state.JobTask → api.JobTaskResponse.
// LeaseToken is intentionally OMITTED — it is an internal
// dispatch primitive (see pkg/sched/lease.go); exposing it on the
// wire would let a customer probe the lease state for a task they
// don't own (cross-tenant enumeration). The error_class is the
// canonical mapped string (succeeded / failed / timeout / oom /
// cancelled / infra) so the dashboard can render the bucket
// without joining events.
//
// Nullable columns (InstanceID / ErrorClass / ErrorMessage /
// ExitCode / StartedAt / FinishedAt) are dereferenced here so
// the wire shape stays flat — the dashboard renders a missing
// error_class as "" (no chip) rather than a JSON null.
func jobTaskResponse(t state.JobTask) api.JobTaskResponse {
	resp := api.JobTaskResponse{
		RunID:     t.RunID,
		TaskIndex: t.TaskIndex,
		Status:    t.Status,
		Attempt:   t.Attempt,
		CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339),
	}
	if t.InstanceID != nil {
		resp.InstanceID = *t.InstanceID
	}
	if t.ErrorClass != nil {
		resp.ErrorClass = *t.ErrorClass
	}
	if t.ErrorMessage != nil {
		resp.ErrorMessage = *t.ErrorMessage
	}
	if t.ExitCode != nil {
		resp.ExitCode = *t.ExitCode
	}
	if t.StartedAt != nil && !t.StartedAt.IsZero() {
		resp.StartedAt = t.StartedAt.UTC().Format(time.RFC3339)
	}
	if t.FinishedAt != nil && !t.FinishedAt.IsZero() {
		resp.FinishedAt = t.FinishedAt.UTC().Format(time.RFC3339)
	}
	return resp
}

// resolveJob is the shared IDOR-safe two-step. Called by every
// per-job read/write handler. Returns:
//
//   - (job, true, nil)       when the slug exists AND the
//                            account_id matches the caller.
//   - (state.Job{}, false, nil) when missing OR cross-account;
//     caller maps both to the same 404 so a probe cannot
//     distinguish the two.
//
// Mirrors the cron precedent at handlers_ext.go::getCron and
// updateCron/deleteCron. The boolean second return distinguishes
// "found" (true) from "absent" (false) for handlers that need
// to emit different audit shapes (e.g. deleteJob uses false to
// short-circuit the soft-delete noise).
func (s *server) resolveJob(ctx context.Context, name string, acct state.Account) (state.Job, bool, error) {
	j, err := s.store.JobGetByName(ctx, acct.ID, name)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return state.Job{}, false, nil
		}
		return state.Job{}, false, err
	}
	if j.AccountID != acct.ID {
		// Defense in depth: JobGetByName is already
		// scoped to (account_id, name), but a future
		// refactor that drops the scope still preserves
		// the cross-tenant guard.
		return state.Job{}, false, nil
	}
	return j, true, nil
}

// resolveJobRun resolves a run to its job+account chain. Two
// queries (run → job → account_id compare) to keep the IDOR
// shape uniform across the family.
func (s *server) resolveJobRun(ctx context.Context, runID string, acct state.Account) (state.Job, state.JobRun, bool, error) {
	run, err := s.store.JobRunGetByID(ctx, runID)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return state.Job{}, state.JobRun{}, false, nil
		}
		return state.Job{}, state.JobRun{}, false, err
	}
	job, err := s.store.JobGetByID(ctx, run.JobID)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return state.Job{}, state.JobRun{}, false, nil
		}
		return state.Job{}, state.JobRun{}, false, err
	}
	if job.AccountID != acct.ID {
		return state.Job{}, state.JobRun{}, false, nil
	}
	return job, run, true, nil
}

// parsePagination extracts ?limit= / ?offset= query parameters
// with the app-list / cron-list clamp (limit ∈ [1, 200], default
// 50; offset ≥ 0, default 0). Shared by listJobs / listJobRuns
// / listJobRunTasks.
func parsePagination(r *http.Request) (limit, offset int) {
	limit = 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 200 {
				n = 200
			}
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	return limit, offset
}

// --- 6 read handlers (Mega-1 M11.2) ----------------------------------

// listJobs handles GET /v1/jobs. Page-based pagination; the
// response's NextOffset=-1 indicates the last page (consistent
// with the apps list endpoint at handlers.go:79).
//
// Plan gating: Free accounts return an empty list (NOT 402 —
// the read gate is not in the plan; the customer is allowed to
// see "no jobs" on a Free plan, the same way a Free customer
// can GET /v1/apps and see an empty list when none are deployed
// below the limit). Write attempts (POST/PATCH/DELETE) hit 402.
//
// Total is computed from JobCountByAccount (cheap, single-row
// SELECT) so the dashboard can render "showing 1-50 of 247"
// without an unbounded count.
func (s *server) listJobs(w http.ResponseWriter, r *http.Request, acct state.Account) {
	limit, offset := parsePagination(r)
	total, err := s.store.JobCountByAccount(r.Context(), acct.ID)
	if err != nil {
		s.log.Error("list jobs: count failed", "account", acct.ID, "err", err)
		api.WriteProblem(w, api.ErrCapacity("could not list jobs"))
		return
	}
	jobs, err := s.store.JobListByAccount(r.Context(), acct.ID, limit, offset)
	if err != nil {
		s.log.Error("list jobs failed", "account", acct.ID, "err", err)
		api.WriteProblem(w, api.ErrCapacity("could not list jobs"))
		return
	}
	out := make([]api.JobResponse, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, jobResponse(j))
	}
	nextOffset := -1
	if offset+len(jobs) < total {
		nextOffset = offset + len(jobs)
	}
	writeJSON(w, http.StatusOK, api.ListJobsResponse{
		Jobs:       out,
		Limit:      limit,
		Offset:     offset,
		NextOffset: nextOffset,
		Total:      total,
	})
}

// getJob handles GET /v1/jobs/{name}. Two-step IDOR-safe lookup:
// resolve by (account_id, name) which already enforces ownership;
// the explicit account_id check is defense in depth.
func (s *server) getJob(w http.ResponseWriter, r *http.Request, acct state.Account) {
	name := r.PathValue("name")
	j, ok, err := s.resolveJob(r.Context(), name, acct)
	if err != nil {
		s.log.Error("get job failed", "name", name, "account", acct.ID, "err", err)
		api.WriteProblem(w, api.ErrCapacity("could not get job"))
		return
	}
	if !ok {
		s.notFound(w, "no such job")
		return
	}
	writeJSON(w, http.StatusOK, jobResponse(j))
}

// listJobRuns handles GET /v1/jobs/{name}/runs. Two-step: resolve
// the job (404 on missing/cross-account), then list runs scoped
// to the job_id. Returns an empty list when the job has no runs
// — distinct from 404 "no such job" — so a fresh job's dashboard
// view renders as "0 runs" rather than an error chip.
func (s *server) listJobRuns(w http.ResponseWriter, r *http.Request, acct state.Account) {
	name := r.PathValue("name")
	job, ok, err := s.resolveJob(r.Context(), name, acct)
	if err != nil {
		s.log.Error("list job runs: resolve failed", "name", name, "account", acct.ID, "err", err)
		api.WriteProblem(w, api.ErrCapacity("could not list runs"))
		return
	}
	if !ok {
		s.notFound(w, "no such job")
		return
	}
	limit, offset := parsePagination(r)
	runs, err := s.store.JobRunListByJob(r.Context(), job.ID, limit, offset)
	if err != nil {
		s.log.Error("list job runs failed", "job", job.ID, "err", err)
		api.WriteProblem(w, api.ErrCapacity("could not list runs"))
		return
	}
	out := make([]api.JobRunResponse, 0, len(runs))
	for _, run := range runs {
		out = append(out, jobRunResponse(run))
	}
	// Run count is per-job (not per-account); the dashboard
	// only needs the visible page + the next-offset hint. No
	// total computation — a job's runs are append-only so an
	// O(runs) count is cheap for hobby (<100) but bounded
	// for Pro (<1000) and Scale (<5000). Defer the unbounded
	// count until the dashboard asks for it.
	nextOffset := -1
	if len(out) == limit {
		nextOffset = offset + len(out)
	}
	writeJSON(w, http.StatusOK, api.ListJobRunsResponse{
		Runs:       out,
		Limit:      limit,
		Offset:     offset,
		NextOffset: nextOffset,
	})
}

// getJobRun handles GET /v1/jobs/{name}/runs/{id}. Two-step via
// resolveJobRun which walks run → job → account_id compare.
func (s *server) getJobRun(w http.ResponseWriter, r *http.Request, acct state.Account) {
	runID := r.PathValue("id")
	_, run, ok, err := s.resolveJobRun(r.Context(), runID, acct)
	if err != nil {
		s.log.Error("get job run failed", "id", runID, "account", acct.ID, "err", err)
		api.WriteProblem(w, api.ErrCapacity("could not get run"))
		return
	}
	if !ok {
		s.notFound(w, "no such run")
		return
	}
	writeJSON(w, http.StatusOK, jobRunResponse(run))
}

// listJobRunTasks handles GET /v1/jobs/{name}/runs/{id}/tasks.
// Walks run → job → account, then lists tasks ordered by
// task_index (the dispatch tick's primary key).
func (s *server) listJobRunTasks(w http.ResponseWriter, r *http.Request, acct state.Account) {
	runID := r.PathValue("id")
	_, _, ok, err := s.resolveJobRun(r.Context(), runID, acct)
	if err != nil {
		s.log.Error("list run tasks: resolve failed", "id", runID, "account", acct.ID, "err", err)
		api.WriteProblem(w, api.ErrCapacity("could not list tasks"))
		return
	}
	if !ok {
		s.notFound(w, "no such run")
		return
	}
	limit, offset := parsePagination(r)
	tasks, err := s.store.JobTaskList(r.Context(), runID, limit, offset)
	if err != nil {
		s.log.Error("list run tasks failed", "run", runID, "err", err)
		api.WriteProblem(w, api.ErrCapacity("could not list tasks"))
		return
	}
	out := make([]api.JobTaskResponse, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, jobTaskResponse(t))
	}
	nextOffset := -1
	if len(out) == limit {
		nextOffset = offset + len(out)
	}
	writeJSON(w, http.StatusOK, api.ListJobTasksResponse{
		Tasks:      out,
		Limit:      limit,
		Offset:     offset,
		NextOffset: nextOffset,
	})
}

// getJobTaskLogs handles GET /v1/jobs/{name}/runs/{id}/tasks/
// {idx}/logs. Two-step: resolve run → account; then resolve
// task by (run_id, task_index); then look up the owning
// instance via the task's instance_id; then proxy to vmmd's
// tail endpoint on the compute node that owns the instance.
//
// Today this returns a stubbed response because the vmmd tail
// proxy surface (issue #572) is shared with the app family
// and not yet wired for jobs. The M11 handler envelope is the
// Mega-1 deliverable — the actual proxy wiring lands in a
// follow-up PR once the vmmd job-instance IPC socket (issue
// #1184 Workstream C) is in place. The handler returns 200
// with empty content + truncated=false so the SDK can already
// round-trip the response shape end-to-end.
//
// Truncated=true means the tail was capped at MaxBytes;
// clients re-fetch with a larger limit to see more. Empty
// LogContent with Truncated=false means the task never produced
// output (process exited before writing anything — common for
// OOM-killed tasks).
func (s *server) getJobTaskLogs(w http.ResponseWriter, r *http.Request, acct state.Account) {
	runID := r.PathValue("id")
	taskIdxStr := r.PathValue("idx")
	taskIdx, err := strconv.Atoi(taskIdxStr)
	if err != nil || taskIdx < 1 {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
			"Invalid task_index", "task_index must be a positive integer"))
		return
	}
	_, _, ok, err := s.resolveJobRun(r.Context(), runID, acct)
	if err != nil {
		s.log.Error("get task logs: resolve failed", "id", runID, "account", acct.ID, "err", err)
		api.WriteProblem(w, api.ErrCapacity("could not get logs"))
		return
	}
	if !ok {
		s.notFound(w, "no such run")
		return
	}
	task, err := s.store.JobTaskGet(r.Context(), runID, taskIdx)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			s.notFound(w, "no such task")
			return
		}
		s.log.Error("get task logs: task lookup failed", "run", runID, "task", taskIdx, "err", err)
		api.WriteProblem(w, api.ErrCapacity("could not get logs"))
		return
	}
	// MaxBytes is bounded at 64 KiB — same shape as the app
	// log endpoint at handlers_ext.go:4000+. Larger reads
	// paginate via ?after=<byte_offset>; not exposed for
	// jobs yet (will land alongside the vmmd IPC follow-up).
	maxBytes := 64 * 1024
	if v := r.URL.Query().Get("max_bytes"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1024*1024 {
			maxBytes = n
		}
	}
	// Stubbed response — see function doc for the vmmd IPC
	// follow-up. Always returns the task's current status
	// so the dashboard can render "exit_code=124 / timeout"
	// even without log content.
	writeJSON(w, http.StatusOK, api.JobTaskLogResponse{
		TaskStatus: task.Status,
		LogContent: "",
		Truncated:  false,
		MaxBytes:   maxBytes,
	})
}