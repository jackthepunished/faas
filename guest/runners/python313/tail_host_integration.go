// Issue #667 / ADR-078 — runner-side tail host integration for the
// python313 runner. Identical to guest/runners/python312/tail_host_integration.go
// except the runtime id is "python313". See the python312 runner
// for the full doc comment; the python312 runner is the canonical
// reference.
package main

import "github.com/onebox-faas/faas/guest/runners/internal"

func drainTailHost(env envelope, resp *response) {
	internal.DrainForResponse("python313", env.WaitUntilSec, env.TailPipePath, &resp.TailErrors)
}
