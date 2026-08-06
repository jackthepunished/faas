// Issue #667 / ADR-078 — runner-side tail host integration for the
// node22 runner. The customer's handler is a node22 script that
// execs the envelope round-trip; the tail host is the runner-side
// pump that drains envelope.TailPipePath after the handler returns
// and emits one 0x04 DGRAM per task via the tail-events proxy.
//
// Where this fits in the request flow:
//
//	handle()
//	  invokeHandler(ctx, handlerPath, env)  // node <script>
//	  → on success:
//	      drainTailHost(env, &resp)         // THIS file
//	      write response envelope to wire
//	      signal.SignalReady(...)           // framework_ready DGRAM
//
// Node22-specific: the customer-side waitUntil integration is
// exposed via __faas_tail.js (preregistered in the runtime image),
// which monkey-patches globalThis.waitUntil. The shim appends one
// JSONL line per waitUntil(promise) registration to
// envelope.TailPipePath. The runner's tail host then drains the
// pipe after invokeHandler returns.
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

// drainTailHost mirrors the go124 runner's shape exactly — the
// JSONL pipe + per-task timeout + 0x04 DGRAM pump is runner-agnostic
// (the runtime-specific bits are the envelope handling, which
// happens in handle()). The runner's taskFn is a no-op closure:
// the customer's promise is on the customer's side (the handler
// subprocess is gone by the time we read); the runner's job is
// to enforce the per-task timeout and emit the 0x04 DGRAM.
//
// Per-#667 acceptance checkbox #7: this function is the
// "waitUntil round-trip" verifier for node22. The
// TestHandle_WaitUntilEnvelopeRoundTrip + the cross-runtime
// TestParity_AllRuntimesHonorWaitUntil pin this sha
// independently.
func drainTailHost(env envelope, resp *response) {
	if env.WaitUntilSec <= 0 || env.TailPipePath == "" {
		return
	}
	host := internal.NewTailHost("node22", env.TailPipePath, env.WaitUntilSec, api.TailCapMax)
	if err := internal.ReadPipe(env.TailPipePath, func(line internal.TailLine) {
		taskFn := func(_ context.Context) {
			// No-op: customer's promise is on the customer side.
		}
		if !host.Register(line.ID, taskFn) {
			resp.TailErrors = append(resp.TailErrors, "dropped:tail_cap_reached:"+line.ID)
		}
	}); err != nil {
		// Pipe read failure (other than not-found, which ReadPipe
		// already gates). The runner keeps draining — a partial
		// drain is better than no drain.
		_ = err
	}
	host.Drain()
	if failures := host.Failures(); len(failures) > 0 {
		resp.TailErrors = append(resp.TailErrors, failures...)
	}
}
