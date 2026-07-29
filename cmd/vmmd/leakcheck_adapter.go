// leakcheck_adapter.go — wires the leakcheck package behind a
// named function so the capacity publisher's `residentFn`
// seam has a stable name to call. The seam is needed so
// tests can inject a stub (cmd/vmmd/capacity_publisher_test.go:
// TestRunCapacityPublish_ResidentBytesSampled) without
// importing the leakcheck package directly.

package main

import "github.com/onebox-faas/faas/pkg/fcvm/leakcheck"

// leakcheckResidentBytes is the production-side adapter: it
// delegates to pkg/fcvm/leakcheck.ResidentBytes. The Boolean
// ok=false on non-Linux hosts is forwarded up so the
// publisher can emit used_mb=0 and let the chooser fall back
// to the store sum (ADR-005).
func leakcheckResidentBytes() (map[string]int64, bool) {
	return leakcheck.ResidentBytes()
}
