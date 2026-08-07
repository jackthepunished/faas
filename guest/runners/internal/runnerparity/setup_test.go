package runnerparity

import (
	"os"
	"testing"

	"github.com/onebox-faas/faas/guest/runners/internal"
)

// TestMain ensures the framework-ready dial hook is installed for the
// runnerparity test binary (e.g. parity_test, tail_test, etc.) and
// restored before exit. The per-runner test binaries install the
// hook themselves via init() in their main_test.go files — see
// guest/runners/node22/main_test.go for the pattern.
//
// Production wiring is unchanged.
func TestMain(m *testing.M) {
	internal.InstallTestProxyDialHook()
	defer internal.SetProxyDialHook(nil) // restore production dialer
	os.Exit(m.Run())
}
