// Package events — job-task audit event family (issue #1184
// Workstream A / ADR-099).
//
// Distinct from cron.fired / trigger.* (ADR-118 ESM vocabulary).
// Job events follow the same shape as the wake/trigger families:
// a stable Kind() constant, a typed payload struct, and a
// Kind/At/Subject/Payload four-method surface that the events
// table writer + the SSE broadcaster both consume.
//
// Emitters:
//   - apid: JobCreated, JobUpdated, JobDeleted
//            (HTTP handler writes; same shape as app/create/update/delete)
//   - schedd dispatchJobsTick: JobTaskDispatched
//            (WakeJob succeeded; mirror of wake.go's WakeEvent
//            but stamped with run_id + task_index + lease_token)
//   - schedd HandleJobExit: JobTaskSucceeded, JobTaskFailed
//            (terminal transition with error_class)
//   - schedd CancelJob/CancelRun/CancelTask: JobRunCancelled
//   - schedd RetryJob/RetryRun/RetryTask: JobTaskRetried
//
// 10 kinds total. Listed below.

package events

import "time"

// TopicJobEvent is the SSE topic Platform publishes job envelopes
// on. Mirrors TopicWake at platform.go:80. The dashboard's job
// surface subscribes to "topic=job" and gets a live feed of
// created/updated/deleted + run lifecycle events.
//
// Distinct from TopicWake so a dashboard subscription to
// "topic=wake" does NOT see job events (jobs aren't wake
// requests; the wake-burst alarm logic would mis-classify them).
const TopicJobEvent = "job"

// JobTask lifecycle kinds. Stable strings — do not rename
// without a dashboard migration. The Kind() method on each
// payload returns one of these constants.
const (
	// JobCreated — apid createJob handler committed. The
	// JobID UUID is the canonical handle for every later event
	// in the lifecycle.
	JobCreated = "job.created"

	// JobUpdated — apid updateJob handler committed (PATCH).
	// Distinct from JobCreated so a dashboard timeline can
	// show "config changed" without conflating with creation.
	JobUpdated = "job.updated"

	// JobDeleted — apid deleteJob handler committed (soft
	// delete; the rows remain for audit + re-create).
	JobDeleted = "job.deleted"

	// JobRunStarted — first task of a run entered the
	// dispatch queue. Aggregate-level, not per-task (we use
	// JobTaskDispatched for the per-task signal).
	JobRunStarted = "job.run.started"

	// JobRunSucceeded — every task in the run settled to
	// status='succeeded'. Aggregate-only.
	JobRunSucceeded = "job.run.succeeded"

	// JobRunFailed — run settled to a terminal-no-retry
	// aggregate status (dead_letter, oom, infra, etc.).
	// Aggregate-only; per-task failure is JobTaskFailed.
	JobRunFailed = "job.run.failed"

	// JobRunCancelled — apid cancelJobRun committed (or
	// schedd cancelRun via internal API). Aggregate-only.
	JobRunCancelled = "job.run.cancelled"

	// JobTaskDispatched — schedd WakeJob successfully minted
	// a lease, marked the task claimed, and called vmmd. The
	// task is now a live microVM. Per-task.
	JobTaskDispatched = "job.task.dispatched"

	// JobTaskSucceeded — HandleJobExit saw exit_code=0;
	// task settled to status='succeeded'. Per-task.
	JobTaskSucceeded = "job.task.succeeded"

	// JobTaskFailed — HandleJobExit saw exit_code!=0 or a
	// non-retryable error class. Settles to status in
	// ('failed','timeout','oom','cancelled'); if retry budget
	// remains, JobTaskRetried fires next.
	JobTaskFailed = "job.task.failed"

	// JobTaskRetried — schedd re-queued the task for the
	// next attempt (capped exponential backoff, see
	// JobTaskRetry). The task transitioned claimed→queued
	// with attempt incremented. Per-task.
	JobTaskRetried = "job.task.retried"
)

// JobCreatedEvent is the typed payload for JobCreated.
// AccountID is the jobs.account_id (the customer owner).
// JobName is the unique-within-account name (jobs.name UNIQUE
// per account_id). ConfigJSON is the canonical config snapshot
// at create time so a dashboard can show the customer what
// they asked for (not the current state, which may have
// drifted via subsequent updates).
type JobCreatedEvent struct {
	JobID      string    `json:"job_id"`
	AccountID  string    `json:"account_id"`
	JobName    string    `json:"job_name"`
	ConfigJSON string    `json:"config_json"`
	CreatedAt  time.Time `json:"created_at"`
}

func (JobCreatedEvent) Kind() string { return JobCreated }
func (e JobCreatedEvent) At() time.Time {
	if e.CreatedAt.IsZero() {
		return time.Now()
	}
	return e.CreatedAt
}
func (JobCreatedEvent) Subject() *string { return nil }
func (e JobCreatedEvent) Payload() map[string]any { return eventPayload(e) }

// JobUpdatedEvent is the typed payload for JobUpdated. Only the
// fields that actually changed are populated (so the "what was
// updated" answer doesn't require diffing the full snapshot).
type JobUpdatedEvent struct {
	JobID         string    `json:"job_id"`
	AccountID     string    `json:"account_id"`
	JobName       string    `json:"job_name"`
	UpdatedAt     time.Time `json:"updated_at"`
	ChangedFields []string  `json:"changed_fields,omitempty"`
}

func (JobUpdatedEvent) Kind() string { return JobUpdated }
func (e JobUpdatedEvent) At() time.Time {
	if e.UpdatedAt.IsZero() {
		return time.Now()
	}
	return e.UpdatedAt
}
func (JobUpdatedEvent) Subject() *string { return nil }
func (e JobUpdatedEvent) Payload() map[string]any { return eventPayload(e) }

// JobDeletedEvent is the typed payload for JobDeleted (soft
// delete). DeletedAt is the jobs.deleted_at timestamp set by
// the handler; the rows remain in the table for audit.
type JobDeletedEvent struct {
	JobID     string    `json:"job_id"`
	AccountID string    `json:"account_id"`
	JobName   string    `json:"job_name"`
	DeletedAt time.Time `json:"deleted_at"`
}

func (JobDeletedEvent) Kind() string { return JobDeleted }
func (e JobDeletedEvent) At() time.Time {
	if e.DeletedAt.IsZero() {
		return time.Now()
	}
	return e.DeletedAt
}
func (JobDeletedEvent) Subject() *string { return nil }
func (e JobDeletedEvent) Payload() map[string]any { return eventPayload(e) }

// JobRunStartedEvent is the typed payload for JobRunStarted.
// Emitted by schedd dispatchJobsTick when the run's first task
// is admitted for dispatch (NOT on createRun — the run is
// "started" when work begins, not when it's defined).
type JobRunStartedEvent struct {
	RunID      string    `json:"run_id"`
	JobID      string    `json:"job_id"`
	AccountID  string    `json:"account_id"`
	JobName    string    `json:"job_name"`
	TotalTasks int       `json:"total_tasks"`
	StartedAt  time.Time `json:"started_at"`
}

func (JobRunStartedEvent) Kind() string { return JobRunStarted }
func (e JobRunStartedEvent) At() time.Time {
	if e.StartedAt.IsZero() {
		return time.Now()
	}
	return e.StartedAt
}
func (JobRunStartedEvent) Subject() *string { return nil }
func (e JobRunStartedEvent) Payload() map[string]any { return eventPayload(e) }

// JobRunSucceededEvent fires when ALL tasks in the run reached
// status='succeeded'. The aggregate counter job_runs.status
// transitions to 'succeeded' in the same transaction (see
// JobRunRecompute in pkg/state/pgstore_jobs.go).
type JobRunSucceededEvent struct {
	RunID         string    `json:"run_id"`
	JobID         string    `json:"job_id"`
	AccountID     string    `json:"account_id"`
	JobName       string    `json:"job_name"`
	SucceededAt   time.Time `json:"succeeded_at"`
	TotalDuration int64     `json:"total_duration_ms"`
}

func (JobRunSucceededEvent) Kind() string { return JobRunSucceeded }
func (e JobRunSucceededEvent) At() time.Time {
	if e.SucceededAt.IsZero() {
		return time.Now()
	}
	return e.SucceededAt
}
func (JobRunSucceededEvent) Subject() *string { return nil }
func (e JobRunSucceededEvent) Payload() map[string]any { return eventPayload(e) }

// JobRunFailedEvent fires when the run settles to a terminal-
// no-retry aggregate status. The aggregate counter job_runs.status
// is one of 'dead_letter', 'cancelled', 'infra', or 'oom'
// (never 'failed' — that's per-task). Cause is the
// canonical error_class string so a dashboard timeline can
// bucket by cause.
type JobRunFailedEvent struct {
	RunID       string    `json:"run_id"`
	JobID       string    `json:"job_id"`
	AccountID   string    `json:"account_id"`
	JobName     string    `json:"job_name"`
	Cause       string    `json:"cause"`
	FailedTasks int       `json:"failed_tasks"`
	FailedAt    time.Time `json:"failed_at"`
}

func (JobRunFailedEvent) Kind() string { return JobRunFailed }
func (e JobRunFailedEvent) At() time.Time {
	if e.FailedAt.IsZero() {
		return time.Now()
	}
	return e.FailedAt
}
func (JobRunFailedEvent) Subject() *string { return nil }
func (e JobRunFailedEvent) Payload() map[string]any { return eventPayload(e) }

// JobRunCancelledEvent fires when apid cancelJobRun committed.
// CancelledAt is the job_runs.cancelled_at timestamp.
type JobRunCancelledEvent struct {
	RunID       string    `json:"run_id"`
	JobID       string    `json:"job_id"`
	AccountID   string    `json:"account_id"`
	JobName     string    `json:"job_name"`
	CancelledAt time.Time `json:"cancelled_at"`
	ActorID     string    `json:"actor_id,omitempty"`
}

func (JobRunCancelledEvent) Kind() string { return JobRunCancelled }
func (e JobRunCancelledEvent) At() time.Time {
	if e.CancelledAt.IsZero() {
		return time.Now()
	}
	return e.CancelledAt
}
func (JobRunCancelledEvent) Subject() *string { return nil }
func (e JobRunCancelledEvent) Payload() map[string]any { return eventPayload(e) }

// JobTaskDispatchedEvent fires when schedd WakeJob successfully
// marked the task claimed and called vmmd. LeaseToken lets a
// dashboard drill-down correlate the event with HandleJobExit's
// terminal event (which carries the same token).
type JobTaskDispatchedEvent struct {
	RunID      string    `json:"run_id"`
	TaskIndex  int       `json:"task_index"`
	JobID      string    `json:"job_id"`
	AccountID  string    `json:"account_id"`
	JobName    string    `json:"job_name"`
	InstanceID string    `json:"instance_id"`
	LeaseToken string    `json:"lease_token"`
	DispatchedAt time.Time `json:"dispatched_at"`
}

func (JobTaskDispatchedEvent) Kind() string { return JobTaskDispatched }
func (e JobTaskDispatchedEvent) At() time.Time {
	if e.DispatchedAt.IsZero() {
		return time.Now()
	}
	return e.DispatchedAt
}
func (JobTaskDispatchedEvent) Subject() *string { return nil }
func (e JobTaskDispatchedEvent) Payload() map[string]any { return eventPayload(e) }

// JobTaskSucceededEvent fires when HandleJobExit saw exit_code=0.
// TaskStatus is always "succeeded" here (the only terminal-no-
// retry status that means "command ran fine"); the field is
// kept for parity with JobTaskFailedEvent so dashboard code
// can use a single dispatch table keyed by kind.
type JobTaskSucceededEvent struct {
	RunID         string    `json:"run_id"`
	TaskIndex     int       `json:"task_index"`
	JobID         string    `json:"job_id"`
	AccountID     string    `json:"account_id"`
	JobName       string    `json:"job_name"`
	InstanceID    string    `json:"instance_id"`
	LeaseToken    string    `json:"lease_token"`
	ExitCode      int32     `json:"exit_code"`
	TaskStatus    string    `json:"task_status"`
	DurationMs    int64     `json:"duration_ms"`
	FinishedAt    time.Time `json:"finished_at"`
}

func (JobTaskSucceededEvent) Kind() string { return JobTaskSucceeded }
func (e JobTaskSucceededEvent) At() time.Time {
	if e.FinishedAt.IsZero() {
		return time.Now()
	}
	return e.FinishedAt
}
func (JobTaskSucceededEvent) Subject() *string { return nil }
func (e JobTaskSucceededEvent) Payload() map[string]any { return eventPayload(e) }

// JobTaskFailedEvent fires on every non-success terminal
// transition. ErrorClass is the canonical mapped string from
// mapExitToTerminalStatus (succeeded/failed/timeout/oom/
// cancelled/infra). ExitCode is the raw guest exit_code from
// JobExitPayload. If retry budget remains, JobTaskRetried fires
// next; the pair lets a dashboard render "failed (will retry
// in 30s)" without joining events.
type JobTaskFailedEvent struct {
	RunID      string    `json:"run_id"`
	TaskIndex  int       `json:"task_index"`
	JobID      string    `json:"job_id"`
	AccountID  string    `json:"account_id"`
	JobName    string    `json:"job_name"`
	InstanceID string    `json:"instance_id"`
	LeaseToken string    `json:"lease_token"`
	ExitCode   int32     `json:"exit_code"`
	ErrorClass string    `json:"error_class"`
	TaskStatus string    `json:"task_status"`
	FinishedAt time.Time `json:"finished_at"`
}

func (JobTaskFailedEvent) Kind() string { return JobTaskFailed }
func (e JobTaskFailedEvent) At() time.Time {
	if e.FinishedAt.IsZero() {
		return time.Now()
	}
	return e.FinishedAt
}
func (JobTaskFailedEvent) Subject() *string { return nil }
func (e JobTaskFailedEvent) Payload() map[string]any { return eventPayload(e) }

// JobTaskRetriedEvent fires when schedd re-queues the task
// after a failure. NextAttemptAt is the absolute timestamp
// at which the dispatch tick will pick it up; the capped
// exponential backoff calculation lives in pkg/sched/jobs.go.
type JobTaskRetriedEvent struct {
	RunID         string    `json:"run_id"`
	TaskIndex     int       `json:"task_index"`
	JobID         string    `json:"job_id"`
	AccountID     string    `json:"account_id"`
	JobName       string    `json:"job_name"`
	Attempt       int       `json:"attempt"`
	NextAttemptAt time.Time `json:"next_attempt_at"`
	RetryAt       time.Time `json:"retry_at"`
}

func (JobTaskRetriedEvent) Kind() string { return JobTaskRetried }
func (e JobTaskRetriedEvent) At() time.Time {
	if e.RetryAt.IsZero() {
		return time.Now()
	}
	return e.RetryAt
}
func (JobTaskRetriedEvent) Subject() *string { return nil }
func (e JobTaskRetriedEvent) Payload() map[string]any { return eventPayload(e) }
