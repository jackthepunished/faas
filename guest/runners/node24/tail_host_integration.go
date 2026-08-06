// Issue #667 / ADR-078 — runner-side tail host integration for the
// node24 runner. Identical to guest/runners/node22/tail_host_integration.go
// except the runtime id is "node24" (used in the wake-tail
// audit row's runtime column). Customer-side waitUntil is exposed
// via __faas_tail.js (preregistered in the node24 runtime image).
//
// See guest/runners/node22/tail_host_integration.go for the full
// doc comment; the node22 runner is the canonical reference.
package main

import "github.com/onebox-faas/faas/guest/runners/internal"

func drainTailHost(env envelope, resp *response) {
	internal.DrainForResponse("node24", env.WaitUntilSec, env.TailPipePath, &resp.TailErrors)
}
