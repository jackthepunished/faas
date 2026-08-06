// Issue #667 / ADR-078 — runner-side tail host integration for the
// python312 runner. The customer's handler is a Python script that
// execs the envelope round-trip; the tail host is the runner-side
// pump that drains envelope.TailPipePath after the handler returns
// and emits one 0x04 DGRAM per task via the tail-events proxy.
//
// See guest/runners/go124/tail_host_integration.go for the canonical
// doc comment; the python312 runner is structurally identical.
package main

import (
	"context"

	"github.com/onebox-faas/faas/guest/runners/internal"
	"github.com/onebox-faas/faas/pkg/api"
)

// drainTailHost runs the per-request tail drain after invokeHandler
// returns. The customer's __faas_tail.py shim (preregistered in
// the runtime image) appends JSONL lines to envelope.TailPipePath
// during the handler's invocation window; the runner's tail host
// emits one 0x04 DGRAM per task via the tail-events proxy.
func drainTailHost(env envelope, resp *response) {
	if env.WaitUntilSec <= 0 || env.TailPipePath == "" {
		return
	}
	host := internal.NewTailHost("python312", env.TailPipePath, env.WaitUntilSec, api.TailCapMax)
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
