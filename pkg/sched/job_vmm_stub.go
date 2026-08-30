// Package sched — fail-open JobVMMClient adapter.
//
// The Mega-1 PR ships the engine-side surface (Engine.WakeJob,
// Engine.DispatchJobsTick, Engine.JobReaperTick, the lease
// primitive, the reaper) but the vmmd gRPC JobColdBoot method is
// not in the proto yet (it lands in a follow-up commit together
// with the BootJob gRPC handler in cmd/vmmd). Until then, every
// BootJob call returns ErrJobVMMNotWired.
//
// schedd's cmd/schedd/main.go wires this adapter via
// Engine.WithJobVmmClient. WakeJob records the error on the run
// as status='failed' (terminal, retryable), and on retry-
// exhaustion flips the run aggregate to 'dead_letter' so the
// customer sees a clear wire response. The audit trail is
// preserved end-to-end.
//
// FAAS_JOBS_DISPATCH=0 keeps the dispatch + reaper tickers off
// in Loop.Run (L.WithJobsDispatched), so this adapter cannot be
// invoked accidentally; the cluster-wide gate stays closed
// while the gRPC surface ships.

package sched

import (
	"context"
	"errors"
	"log/slog"
)

// ErrJobVMMNotWired is returned by FailOpenJobVMMClient.JobColdBoot
// until the vmmd gRPC JobColdBoot method lands. Stable sentinel
// for errors.Is checks in cmd/apid/handlers_jobs.go + the
// WakeJob error-classification branch in pkg/sched/jobs.go.
var ErrJobVMMNotWired = errors.New("sched: job vmm gRPC surface not wired (Mega-1 follow-up)")

// ErrJobLeaserNil is returned by Engine.WakeJob when the engine's
// jobLeaser field is nil (production schedd leaves it nil until
// the PgLeaser surface is unified with *pgxpool.Pool in Mega-1.5).
// Stable sentinel — same classification contract as ErrJobVMMNotWired.
var ErrJobLeaserNil = errors.New("sched: job leaser not wired (Mega-1 follow-up)")

// FailOpenJobVMMClient is the production no-op jobVmmClient adapter
// schedd wires in until vmmd exposes a JobColdBoot gRPC method.
// Every call returns (zero-value, ErrJobVMMNotWired). The engine
// catches the sentinel and routes the run to status='failed'
// (terminal, retryable) so the customer sees a CodeJobVMMUnavailable
// 503 on POST /v1/jobs/{name}/runs, not a nil-panic or a silent
// lease-loss loop.
//
// Why fail-open instead of nil (which would nil-deref in
// pkg/sched/jobs.go:158):
//
//   - nil-deref masks the issue behind a "wake panic" stack,
//     which is impossible to root-cause without a debugger
//     attached.
//   - A sentinel error flows through dispatchJobsTick's existing
//     classify-or-retry path, surfaces in `usage_daily` as a
//     dead-letter row, and pings the `jobs_dispatch_rejected`
//     Prometheus counter so the operator sees the dispatch is
//     turned off in observability.
//
// The follow-up commit (Mega-1.5) replaces this adapter with a
// real vmmd gRPC client that issues BootColdBootForJob + the
// JobExitNotification DGRAM listener wired back into
// Engine.HandleJobExit.
type FailOpenJobVMMClient struct {
	log *slog.Logger
}

// NewFailOpenJobVMMClient returns a no-op jobVmmClient that surfaces
// ErrJobVMMNotWired on every JobColdBoot call. Construction is
// cheap; the adapter is wired unconditionally so FAAS_JOBS_DISPATCH
// can be flipped at runtime without restarting schedd.
func NewFailOpenJobVMMClient(log *slog.Logger) *FailOpenJobVMMClient {
	if log == nil {
		log = slog.Default()
	}
	return &FailOpenJobVMMClient{log: log}
}

// JobColdBoot satisfies jobVmmClient (Engine.WakeJob line 158).
// Returns ErrJobVMMNotWired so the engine can classify the run as
// failed → retryable. Logs ONCE per (runID, taskIndex) pair via
// the engine's per-task de-dupe to avoid log floods during the
// dispatch tick.
func (c *FailOpenJobVMMClient) JobColdBoot(ctx context.Context, spec JobVmmSpec) (JobVmmResult, error) {
	c.log.Warn("schedd: JobColdBoot invoked but vmmd gRPC not wired yet",
		"run_id", spec.RunID,
		"task_index", spec.TaskIndex,
		"account_id", spec.AccountID,
		"hint", "FAAS_JOBS_DISPATCH=1 set; Mega-1.5 follow-up will ship the vmmd proto")
	return JobVmmResult{}, ErrJobVMMNotWired
}
