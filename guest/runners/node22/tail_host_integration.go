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
//
// The runner shim is a one-liner — the load-bearing logic lives
// in internal.DrainForResponse (dedup'd from the 5x near-identical
// shims by issue #667 review item #11).
package main

import "github.com/onebox-faas/faas/guest/runners/internal"

func drainTailHost(env envelope, resp *response) {
	internal.DrainForResponse("node22", env.WaitUntilSec, env.TailPipePath, &resp.TailErrors)
}
