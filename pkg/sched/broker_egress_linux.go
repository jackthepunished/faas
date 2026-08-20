//go:build linux

// broker_egress_linux.go — Linux implementation of the broker
// egress accounting surface (issue #757 / ADR-118, commit 8).
//
// Build-tag = linux only. On darwin (Lima metal-lima runs
// against an arm64 Linux guest, but the developer's macOS
// build doesn't have /dev/kvm; `make test` runs the host's
// darwin build path) the constructor returns a noop, which
// means the unit tests covering the noop + Linux-argv shape
// are the only verification surface in this repo.
//
// Activation: the tc qdisc argv returned by BrokerTcCommands
// is what the systemd unit's ExecStartPre runs the first
// time faas-brokerq.slice brings the brokerq host interface
// up. The runtime side (NewLinuxBrokerAccountor) wires the
// Account method to an atomic counter — the cap enforcement
// lives at the kernel qdisc, not in the Go runtime; the Go
// side is observability-only.
//
// The `tbf` qdisc (mirrors pkg/netns/config.go::TcCommands
// for per-VM egress) is the right shape for a single-link
// egress pipe that all of schedd's poll goroutines share.
// Burst 32kbit / latency 400ms match the per-VM defaults
// from pkg/netns — same rationale (covers ~4× the smallest
// rate's packet-per-MTU inside tbf's limit ceiling).

package sched

import (
	"context"
	"fmt"
)

// NewLinuxBrokerAccountor returns the Linux-side BrokerAccountor.
// On every Account call, it increments the internal atomic counter
// (exposed via pkg/sched metrics in commit 9). The actual byte
// cap is enforced at the kernel qdisc level — the Go runtime
// is observability-only.
//
// Returns a non-nil BrokerAccountor even on non-Linux builds,
// but the underlying constructor (this function) is build-tag-
// gated and is unreachable from non-linux builds because
// NewBrokerAccountor's last branch (EgressMbit > 0) calls it.
func NewLinuxBrokerAccountor(cfg BrokerEgressConfig) BrokerAccountor {
	if cfg.EgressMbit <= 0 {
		return noopBrokerAccountor{}
	}
	obs := newBrokerEgressObserver()
	return &linuxBrokerAccountor{
		iface:      cfg.InterfaceName,
		egressMbit: cfg.EgressMbit,
		observer:   obs,
	}
}

// linuxBrokerAccountor is the Linux-only BrokerAccountor. Per-
// tick Account increments the observer; the kernel qdisc does
// the actual byte-cap enforcement.
type linuxBrokerAccountor struct {
	iface      string
	egressMbit int
	observer   *brokerEgressObserver
}

func (l *linuxBrokerAccountor) Account(_ context.Context, _ string, bytes int64) {
	if bytes <= 0 {
		return
	}
	l.observer.Account(bytes)
}

func (l *linuxBrokerAccountor) Close() error { return nil }

// BrokerTcCommands returns the argv list that applies the
// per-deployment broker egress bandwidth cap on the brokerq
// host interface. Mirrors the shape of
// pkg/netns/config.go::TcCommands (single-argv tbf on the
// link), with rate substituted from the plan cap.
//
// The systemd unit faas-brokerq.slice runs this argv through
// ExecStartPre so the cap is in place before schedd starts
// dispatching. A second invocation on the same interface
// would fail with EEXIST — the unit only runs these once
// per boot; for per-trigger re-caps (e.g. a re-deploy that
// changes the customer's plan), the broker_egress runtime
// records the count and reports it via metrics rather than
// re-running tc.
//
// EgressMbit == 0 returns nil so the systemd unit can skip
// the ExecStartPre entirely (matches pkg/netns's EgressMbit
// == 0 contract).
func BrokerTcCommands(cfg BrokerEgressConfig) [][]string {
	if cfg.EgressMbit <= 0 || cfg.InterfaceName == "" {
		return nil
	}
	return [][]string{
		{"tc", "qdisc", "add", "dev", cfg.InterfaceName, "root", "tbf",
			"rate", fmt.Sprintf("%dmbit", cfg.EgressMbit),
			"burst", "32kbit", "latency", "400ms"},
	}
}
