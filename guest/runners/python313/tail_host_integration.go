// Issue #667 / ADR-078 — runner-side tail host integration for the
// python313 runner. Identical to guest/runners/python312/tail_host_integration.go
// except the runtime id is "python313". See the python312 runner
// for the full doc comment; the python312 runner is the canonical
// reference.
package main

import (
	"context"

	"github.com/onebox-faas/faas/guest/runners/internal"
	"github.com/onebox-faas/faas/pkg/api"
)

// drainTailHost runs the per-request tail drain after invokeHandler
// returns. The runner reads envelope.TailPipePath (the JSONL pipe
// the customer's __faas_tail.py shim wrote to during the handler
// invocation window) and emits one 0x04 DGRAM per task via the
// tail-events proxy.
func drainTailHost(env envelope, resp *response) {
	if env.WaitUntilSec <= 0 || env.TailPipePath == "" {
		return
	}
	host := internal.NewTailHost("python313", env.TailPipePath, env.WaitUntilSec, api.TailCapMax)
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
