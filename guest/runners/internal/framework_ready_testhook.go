package internal

import (
	"net"
	"sync"
)

// testProxyDialHookInstalled guards the one-shot installation of
// the framework-ready dial hook. Each runner package's test binary
// links internal as an import, and a TestMain or init() in those
// tests calls InstallTestProxyDialHook once per binary. The
// sync.Once here is belt-and-braces: double installs are no-ops.
var testProxyDialHookInstalled sync.Once

// InstallTestProxyDialHook installs a stub dialer that returns a
// synthetic ENOENT-style error in microseconds (instead of the 250ms
// Dialer.Timeout a real unix-socket dial would burn against
// /run/guest-init/framework-ready.sock on a Mac/Linux test box
// where that path is absent). The error is absorbed by the
// sync.Once fence at signalFrameworkReady's caller; the runner's
// request handling proceeds normally.
//
// Production wiring is unchanged: callers that don't run this hook
// see the production net.Dialer's timeout-bounded behavior.
//
// Why this lives in package internal (not in a *_test.go file):
// test files are only compiled into their own package's test
// binary, not into dependent packages' test binaries. Every runner
// test imports package internal, so a non-test helper exposed
// here is reachable from each runner package's init() without a
// TestMain in every package.
func InstallTestProxyDialHook() {
	testProxyDialHookInstalled.Do(installTestProxyDialHook)
}

func installTestProxyDialHook() {
	SetProxyDialHook(func(_, _ string) (net.Conn, error) {
		return nil, errNoProxy
	})
}

// errNoProxy is the sentinel the test dialer returns. Private
// because it must never collide with a real filesystem error in
// production logs.
var errNoProxy = errNoProxyT{}

type errNoProxyT struct{}

func (errNoProxyT) Error() string { return "framework-ready proxy disabled in unit tests" }
