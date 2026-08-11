//go:build !linux

//nolint:unused

// Non-linux stub (issue #463 / ADR-069 / ADR-071 / PR-C). The
// linux proxy uses unix.AF_VSOCK + unix.SOCK_DGRAM, which
// doesn't exist off the linux kernel. The test binary that
// runs on darwin via `go test` consumes the types below; the
// production code path (which is always linux — guest-init is
// the in-guest PID 1 of every microVM) takes the
// sidecar_events_proxy_linux.go build. The "no signal"
// contract is preserved by returning nil from every method on
// a nil receiver.
//
//nolint:unused
package main

import "log/slog"

// startSidecarEventsProxy is a no-op on non-linux. The
// returned proxy has a nil fd so all sends short-circuit
// silently — matching the "no signal" contract documented in
// sidecar_events_proxy_linux.go.
func startSidecarEventsProxy(log *slog.Logger) (*sidecarEventsProxy, error) {
	return nil, nil
}

// sidecarEventsProxy is a type alias on non-linux so the
// package's symbol surface matches the linux build. The
// receiver methods are the linux-build implementations; this
// stub exists so cross-build import resolution is consistent.
type sidecarEventsProxy struct{}
