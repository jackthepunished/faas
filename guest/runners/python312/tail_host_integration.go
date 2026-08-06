// Issue #667 / ADR-078 — runner-side tail host integration for the
// python312 runner. The customer's handler is a Python script that
// execs the envelope round-trip; the tail host is the runner-side
// pump that drains envelope.TailPipePath after the handler returns
// and emits one 0x04 DGRAM per task via the tail-events proxy.
//
// See guest/runners/go124/tail_host_integration.go for the canonical
// doc comment; the python312 runner is structurally identical.
package main

import "github.com/onebox-faas/faas/guest/runners/internal"

func drainTailHost(env envelope, resp *response) {
	internal.DrainForResponse("python312", env.WaitUntilSec, env.TailPipePath, &resp.TailErrors)
}
