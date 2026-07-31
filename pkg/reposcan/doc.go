// Package reposcan — repo-decomposition loader (ADR-050 / impl plan §3).
//
// Pure: no network, no exec, no Postgres. Tested with fstest.MapFS.
// Returns a deterministic, sorted Result (sorted by Name) so the
// confirm table in Phase 3 and its golden fixture tests stay stable.
//
// On any fs.FS error inside Scan, a missing or unparseable source
// file is skipped with a Warning; the rest of the scan completes.
// The only Scan-time error is fs.ValidPath failure: callers must
// not be able to read outside the archive root. That constraint
// lives in fsysSafety.go's readValidFile and is the load-bearing
// security claim — symlinks and ".." escapes never reach the host.
package reposcan
