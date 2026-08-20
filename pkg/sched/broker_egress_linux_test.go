//go:build linux

// broker_egress_linux_test.go — Linux-only argv-shape tests
// for the broker egress accounting surface (issue #757 /
// ADR-118, commit 8).
//
// The cross-build test file (broker_egress_test.go) pins the
// stub contract; this file pins the real tc qdisc argv that
// the systemd unit's ExecStartPre runs at first boot.

package sched

import (
	"context"
	"strings"
	"testing"
)

// TestBrokerTcCommands_ARGVShape pins the tc qdisc argv. The
// shape mirrors pkg/netns/config.go::TcCommands (single-argv
// tbf on the link) — a future refactor that swaps tbf for an
// htb-class hierarchy would regress tests like
// pkg/netns/config_test.go::TestTcCommandsApplyRateToVethHost,
// AND this test. Pinning here keeps the brokerq shape visible
// from this file's blast radius.
func TestBrokerTcCommands_ARGVShape(t *testing.T) {
	t.Parallel()
	cmds := BrokerTcCommands(BrokerEgressConfig{
		InterfaceName: "br-brokerq",
		EgressMbit:    200,
	})
	if len(cmds) != 1 {
		t.Fatalf("cmds = %d argvs, want 1: %v", len(cmds), cmds)
	}
	got := strings.Join(cmds[0], " ")
	want := "tc qdisc add dev br-brokerq root tbf rate 200mbit burst 32kbit latency 400ms"
	if got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
}

// TestBrokerTcCommands_NilWhenCapZero pins the zero-cap
// contract: BrokerTcCommands MUST return nil so the systemd
// unit's ExecStartPre skips the line entirely (matches the
// pkg/netns EgressMbit == 0 contract — the absolute refusal
// rather than the silent-empty-shape that would be a footgun
// at runtime). On linux this contracts the real implementation;
// the stub counterpart is pinned in broker_egress_test.go.
func TestBrokerTcCommands_NilWhenCapZero(t *testing.T) {
	t.Parallel()
	for _, cfg := range []BrokerEgressConfig{
		{InterfaceName: "br-brokerq", EgressMbit: 0},
		{InterfaceName: "br-brokerq", EgressMbit: -1},
		{InterfaceName: "", EgressMbit: 200},
		{},
	} {
		cfg := cfg
		if got := BrokerTcCommands(cfg); got != nil {
			t.Errorf("BrokerTcCommands(%+v) = %v, want nil", cfg, got)
		}
	}
}

// TestLinuxBrokerAccountor_NilSafe pins the Linux-side
// constructor: a non-positive EgressMbit OR empty
// InterfaceName collapses to noopBrokerAccountor. The real
// constructor must NOT fall through to a state where the cap
// is silently dropped at the kernel level.
func TestLinuxBrokerAccountor_NilSafe(t *testing.T) {
	t.Parallel()
	for _, cfg := range []BrokerEgressConfig{
		{InterfaceName: "br-brokerq", EgressMbit: 0},
		{InterfaceName: "br-brokerq", EgressMbit: -1},
		{InterfaceName: "", EgressMbit: 200},
		{},
	} {
		cfg := cfg
		acc := NewLinuxBrokerAccountor(cfg)
		if acc == nil {
			t.Errorf("NewLinuxBrokerAccountor(%+v) = nil, want non-nil noop fallback", cfg)
		}
		// Noop contract: Account is a no-op, Close is nil.
		acc.Account(context.Background(), "t", 1024)
		if err := acc.Close(); err != nil {
			t.Errorf("Close err = %v, want nil", err)
		}
	}
}
