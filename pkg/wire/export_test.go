// Test-only exports for pkg/wire (issue #518 PR-A). These symbols are
// exposed only when this file is compiled, i.e. under `go test`. The
// production API surface stays identical — production callers have no
// way to mutate the shared leveler or invoke the SIGHUP goroutine;
// only the SIGHUP signal path can.
//
// Per whitebox-test-file-pattern.md: narrowly-scoped white-box surface
// area for tests that need to reach into package-internal state. Test
// files do not contribute to the production binary's symbol table.

package wire

import (
	"context"
	"log/slog"
	"os"
)

// WatchLogLevelReload exposes the SIGHUP-driven reload goroutine for
// direct test driving. Production callers must use Daemon(), which wires
// the same function through signal.Notify. The signature is identical
// to the internal helper.
func WatchLogLevelReload(ctx context.Context, log *slog.Logger, hupCh <-chan os.Signal, getenv func(string) string) {
	watchLogLevelReload(ctx, log, hupCh, getenv)
}

// SetLogLevelForTest mutates the shared slog.LevelVar directly. Tests
// use this to simulate what a SIGHUP-driven re-read of FAAS_LOG_LEVEL
// would do in production; the production path is to send the daemon a
// SIGHUP and let watchLogLevelReload pick up the change via env. There
// is no race with the production handler in tests because tests own
// the goroutine that mutates the leveler.
func SetLogLevelForTest(lvl slog.Level) {
	logLevel.Set(lvl)
}

// LogLevelForTest returns the current value of the shared leveler. Used
// by tests that need to assert post-SIGHUP state without re-reading
// the env.
func LogLevelForTest() slog.Level {
	return logLevel.Level()
}
