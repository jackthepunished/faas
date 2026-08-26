// MemStore stubs for the per-app observability counters used by the
// customer-facing per-app dashboard (commit 2 of the per-app obs PR
// series). The production path is Postgres via the sqlc-generated
// queries; MemStore exists for unit tests + local development.
//
// Every method here returns the empty-zero value + a sentinel
// "not implemented" error so the test suite fails loudly if a unit
// test exercises this code path against MemStore (the right answer
// is to mark the test //go:build metal or run it against the pgtest
// harness instead — same posture as memstore_app_errors.go for
// ADR-096 and memstore_request_telemetry.go for ADR-127).

package state

import (
	"context"
	"errors"
)

// errMemStoreWakeCount is the sentinel returned by every per-app
// observability counter stub on MemStore. Catching this in a unit
// test means the test should be //go:build metal (run against
// pgtest) rather than MemStore — the underlying query joins the
// events table on a jsonb expression index that MemStore does not
// model.
var errMemStoreWakeCount = errors.New("state: MemStore does not implement per-app wake counts — run the test against pgtest")

// CountWakeBootStarted24h (per-app dashboard, Hobby+) — MemStore
// stub. Postgres-only. Returns the zero value plus a sentinel
// error so a unit test that exercises the per-app dashboard's
// wakes_24h field against MemStore surfaces the sentinel rather
// than silently returning 0 (a 0 result is indistinguishable
// from a fresh app with no traffic, and a wrong-zero would mask
// the test bug). The handler is best-effort: 0 + nil error
// degrades to "no wakes recorded today"; 0 + sentinel error
// surfaces as the existing `s.log.Warn("wakes_24h fetch failed",
// ...)` line in cmd/apid/handlers_metrics.go's enrichment loop.
func (m *MemStore) CountWakeBootStarted24h(_ context.Context, _ string) (int64, error) {
	return 0, errMemStoreWakeCount
}
