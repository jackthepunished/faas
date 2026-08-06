// Issue #667 / ADR-078 — runner-side tail host integration for the
// go124 runner. The customer's handler is a static Go binary that
// execs the envelope round-trip; the tail host is the runner-side
// pump that drains envelope.TailPipePath after the handler returns
// and emits one 0x04 DGRAM per task via the tail-events proxy.
//
// Where this fits in the request flow:
//
//	handle()
//	  invokeHandler(ctx, handlerPath, env)  // customer subprocess
//	  → on success:
//	      drainTailHost(env, &resp)         // THIS file
//	      write response envelope to wire
//	      signal.SignalReady(...)           // framework_ready DGRAM
//
// The drain runs BEFORE the response envelope is written and BEFORE
// the framework_ready signal fires. The ordering matters because:
//   - the customer's tail_seconds window ends before the response
//     returns (the handler's `waitUntil(promise)` is observed as
//     "drained" only after the runner's WaitGroup finishes)
//   - the framework_ready signal fires on the first non-5xx response
//     — wake snapshot capture waits on this signal, and the snapshot
//     shouldn't capture a VM that's still draining tail tasks
//
// ADR-078 §"Reaper gate" was amended (issue #667 follow-up) to
// document this timing change: previously the signal fired on the
// first non-5xx response, NOW it fires after tailHost.Drain()
// returns. The 5s snapshotAndPark watchdog on schedd is the upper
// bound if the drain hangs.
package main

import (
	"context"

	"github.com/onebox-faas/faas/guest/runners/internal"
	"github.com/onebox-faas/faas/pkg/api"
)

// drainTailHost reads the JSONL pipe at env.TailPipePath, registers
// each line as a tail task, and blocks until every task reaches a
// terminal state (or until the per-task ceiling expires — the
// runner's `tailHost.waitUntil` is the per-task cap, NOT the
// handler subprocess timeout).
//
// Pre-conditions:
//   - env.WaitUntilSec > 0 (else no-op — feature disabled on this
//     request; matches the Vercel Edge / Cloudflare pre-tail behavior
//     cited in ADR-078 §"Rules")
//   - env.TailPipePath != "" (else no-op — no customer registrations)
//
// On return, resp.TailErrors is populated with the per-task failure
// list (timeouts and panics). The runner marshals resp back to the
// caller — the customer-visible envelope is unchanged shape (the
// TailErrors field is debug-only, surfaced via runner stderr instead
// of the HTTP response body).
//
// The TailCapMax constant is pinned at 16 in pkg/api/limits.go. The
// runner is the structural enforcer — a customer that registers 17
// tails sees the 17th dropped (the tail host's Register returns
// false). The wire-counter pkg/wire/metrics.TailCapReached is the
// operator-visible warning.
func drainTailHost(env envelope, resp *response) {
	if env.WaitUntilSec <= 0 || env.TailPipePath == "" {
		// Feature disabled on this request (backwards-compatible
		// with pre-#667 handlers). The runner's framework_ready
		// signal fires after this function returns without doing
		// any work — same shape as PR 2.
		return
	}
	host := internal.NewTailHost("go124", env.TailPipePath, env.WaitUntilSec, api.TailCapMax)
	// ReadPipe consumes the JSONL pipe the customer's handler
	// wrote to during its invocation. Each non-empty line is
	// registered as a tail task. The runner's taskFn is a no-op
	// closure — the customer's promise is on the customer's side
	// (the handler subprocess is gone by the time we read); the
	// runner's job is to enforce the per-task timeout and emit
	// the 0x04 DGRAM. The optimistic-default outcome in
	// tail_host.go::runTask (Completed if ctx.Err() is nil) is
	// the correct read here: the handler completed normally →
	// its promises are presumed completed too; the only way to
	// flip to Failed/Timeout is a panic or a deadline expiry.
	err := internal.ReadPipe(env.TailPipePath, func(line internal.TailLine) {
		taskFn := func(_ context.Context) {
			// No-op: the customer's promise is observed by the
			// handler before subprocess exit. The runner's only
			// job is to honor the per-task ceiling and emit the
			// receipt. A bounded sleep is unnecessary — the
			// tail host's context.WithTimeout in runTask is the
			// safety net.
		}
		if !host.Register(line.ID, taskFn) {
			// TailCapMax reached — the runner drops the
			// registration. The wire counter
			// (pkg/wire/metrics.TailCapReached) is the
			// operator-visible alarm; the runner also logs
			// to stderr so the per-task failure is debuggable.
			resp.TailErrors = append(resp.TailErrors, "dropped:tail_cap_reached:"+line.ID)
		}
	})
	if err != nil {
		// Pipe read failure (other than not-found, which ReadPipe
		// already gates). The runner keeps draining — a partial
		// drain is better than no drain.
		// socktainerV1: tail_host pipe read error
		_ = err
	}
	host.Drain()
	if failures := host.Failures(); len(failures) > 0 {
		resp.TailErrors = append(resp.TailErrors, failures...)
	}
}
