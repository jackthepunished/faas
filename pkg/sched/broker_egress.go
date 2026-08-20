// broker_egress.go — broker-egress accounting for the kafka /
// nats / redis / sqs poller goroutines
// (issue #757 / ADR-118, commit 8 of feat-triggers-mega).
//
// Schedd's broker poll goroutines fan out from the per-trigger
// Loop into a per-broker connection pool. The financial model
// (§1) budgets the broker egress as part of "free tenant
// egress", but the size of the broker-side data plane is
// uncorrelated with the customer's per-app egress — a tenant
// running one high-volume Kafka trigger can saturate the
// control-plane's broker pipe without their per-app egress
// cap firing. The BrokerEgressMbit plan cap is the answer:
// one tc qdisc on the broker-egress host interface, summed
// across all of schedd's poll goroutines. This file is the
// accounting side; the tc-shape is in broker_egress_linux.go.
//
// Why an interface rather than a free function: the runtime
// layer is hot (called per dispatch tick per trigger). We
// want the noop path to be a single interface indirection
// without conditional branches on the dispatch hot path. The
// Linux implementation is build-tag-gated; on darwin (Lima
// metal-lima runs on macOS, where the build is wired to
// darwin tags) the constructor returns the noop so unit tests
// still run.
//
// This commit ships the runtime + noop shape + the linux
// build-tag stub. The actual activation (running the tc
// commands at first-EgressMbit > 0) is performed by the
// systemd unit `faas-brokerq.slice`, which is added to
// pkg/daemonunitspec in this commit. The activation path is
// deliberately deferred to a follow-up: the EX44 control
// plane is the only host with the systemd version that
// supports IOBandwidthMax (per ADR-118 §"Risks, fallback
// iptables"), and we don't want to gate commit 8 on that
// verification.

package sched

import (
	"context"
	"sync/atomic"
)

// BrokerAccountor is the per-tick broker egress accounting
// surface that pkg/sched/dispatch_triggers.go calls into after
// every Poll. Implementations:
//
//   - noopBrokerAccountor: zero-cost; the default on darwin /
//     non-linux builds, and the fallback when EgressMbit == 0
//     on linux (no shaper is needed).
//   - linuxBrokerAccountor: per-goroutine byte counter that
//     the linux-side tc shaper enforces on the brokerq
//     host interface. Wired in commit 8 follow-up activation.
//
// All methods MUST be safe to call concurrently from multiple
// dispatch ticks.
type BrokerAccountor interface {
	// Account is called once per dispatch tick with the byte
	// count the dispatcher is about to push to the broker.
	// Implementations may accumulate the count, drop it, or
	// surface it via Prometheus; the contract is "fire and
	// forget — don't block".
	Account(ctx context.Context, triggerID string, bytes int64)

	// Close releases any resources (e.g. the linux-side
	// counter file descriptor). Safe to call multiple times.
	Close() error
}

// noopBrokerAccountor is the default BrokerAccountor when
// the linux implementation is unavailable or the EgressMbit
// cap is zero. Zero-cost in the common (non-linux) case.
type noopBrokerAccountor struct{}

func (noopBrokerAccountor) Account(_ context.Context, _ string, _ int64) {}
func (noopBrokerAccountor) Close() error                                 { return nil }

// BrokerEgressConfig is the per-deployment wire shape for the
// broker egress layer. Read from the schedd config TOML
// (FAAS_BROKER_EGRESS_MBIT env var consumed by the systemd
// unit). nil BrokersInterface + zero EgressMbit means "no
// shaper, use the noop accountor".
type BrokerEgressConfig struct {
	// InterfaceName is the host-side interface the broker-q
	// qdisc lives on (e.g. "br-brokerq" in the production
	// deploy, or a dummy "dummy-brokerq0" for unit tests).
	// Empty + zero EgressMbit = noopBrokerAccountor.
	InterfaceName string
	// EgressMbit is the per-deployment broker egress cap
	// (sum of bytes across all poll goroutines). Matches the
	// largest plan's cap (Scale = 200 Mbit/s). A zero value
	// disables the shaper and selects noopBrokerAccountor.
	EgressMbit int
}

// NewBrokerAccountor picks the right BrokerAccountor based on
// the build tag + config. Builds excluding
// broker_egress_linux.go get noopBrokerAccountor.
//
// Returns (noopBrokerAccountor{}, nil) when cfg is nil or
// EgressMbit == 0 — the absence of a shaper is the steady-state
// for Hobby and lower plans that don't carry a broker quota
// in the financial model.
func NewBrokerAccountor(cfg BrokerEgressConfig) BrokerAccountor {
	if cfg.EgressMbit <= 0 || cfg.InterfaceName == "" {
		return noopBrokerAccountor{}
	}
	return NewLinuxBrokerAccountor(cfg)
}

// brokerEgressObserver is the optional Prometheus sidecar the
// Linux implementation wires up. Exposed here so unit tests
// can pin "Account increments the live counter" without
// spinning up a real registry.
//
// production_egress_total_bytes is the per-deployment bytes
// counter; the per-trigger counter comes from the existing
// schedd_esm_* metrics in pkg/wire/metrics.go (commit 9).
type brokerEgressObserver struct {
	totalBytes atomic.Int64
}

func newBrokerEgressObserver() *brokerEgressObserver {
	return &brokerEgressObserver{}
}

func (b *brokerEgressObserver) Account(n int64) {
	b.totalBytes.Add(n)
}

func (b *brokerEgressObserver) TotalBytes() int64 {
	return b.totalBytes.Load()
}
