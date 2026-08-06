// Issue #667 / ADR-078 — runner-side tail host integration for the
// node24 runner. Identical to guest/runners/node22/tail_host_integration.go
// except the runtime id is "node24" (used in the wake-tail
// audit row's runtime column). Customer-side waitUntil is exposed
// via __faas_tail.js (preregistered in the node24 runtime image).
//
// See guest/runners/node22/tail_host_integration.go for the full
// doc comment; the node22 runner is the canonical reference.
package main

import (
	"context"

	"github.com/onebox-faas/faas/guest/runners/internal"
	"github.com/onebox-faas/faas/pkg/api"
)

// drainTailHost runs the per-request tail drain after invokeHandler
// returns. The runner reads envelope.TailPipePath (the JSONL pipe
// the customer's __faas_tail.js shim wrote to during the handler
// invocation window) and emits one 0x04 DGRAM per task via the
// tail-events proxy.
func drainTailHost(env envelope, resp *response) {
	if env.WaitUntilSec <= 0 || env.TailPipePath == "" {
		return
	}
	host := internal.NewTailHost("node24", env.TailPipePath, env.WaitUntilSec, api.TailCapMax)
	if err := internal.ReadPipe(env.TailPipePath, func(line internal.TailLine) {
		taskFn := func(_ context.Context) {
			// No-op: customer's promise is on the customer side.
		}
		if !host.Register(line.ID, taskFn) {
			resp.TailErrors = append(resp.TailErrors, "dropped:tail_cap_reached:"+line.ID)
		}
	}); err != nil {
		_ = err
	}
	host.Drain()
	if failures := host.Failures(); len(failures) > 0 {
		resp.TailErrors = append(resp.TailErrors, failures...)
	}
}
