//go:build !linux

// broker_egress_stub.go — non-linux build of the broker
// egress accounting surface (issue #757 / ADR-118, commit 8).
//
// On darwin / other non-linux builds, NewLinuxBrokerAccountor
// is unreachable (NewBrokerAccountor returns the noop for
// EgressMbit > 0). This file defines the symbol so any
// accidental cross-build import doesn't fail the build with
// "undefined: NewLinuxBrokerAccountor".

package sched

// NewLinuxBrokerAccountor returns noopBrokerAccountor on non-linux
// builds. The build-tag-gated linux implementation in
// broker_egress_linux.go owns the real surface.
func NewLinuxBrokerAccountor(_ BrokerEgressConfig) BrokerAccountor {
	return noopBrokerAccountor{}
}

// BrokerTcCommands returns nil on non-linux builds. The
// build-tag-gated Linux implementation owns the real argv.
func BrokerTcCommands(_ BrokerEgressConfig) [][]string {
	return nil
}
