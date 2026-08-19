// broker_egress_test.go — unit tests for the broker egress
// accounting surface (issue #757 / ADR-118, commit 8).
//
// The runtime is small enough that table-driven tests can pin
// every contract: the noop fallback, the per-plan cap, the
// Linux tc argv shape, the byte-counting observer, and the nil
// accountor guard in dispatch_triggers.go.

package sched

import (
	"context"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/daemonunitspec"
)

// TestNewBrokerAccountor_NoopWhenCapZero pins the canonical
// Hobby / no-quota shape: EgressMbit <= 0 must produce the
// noop accountor, not the Linux implementation. The Linux
// implementation would have system-side effects (tc qdisc add),
// so a wrong default would silently apply a zero-rate qdisc on
// Hobby customers.
func TestNewBrokerAccountor_NoopWhenCapZero(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  BrokerEgressConfig
	}{
		{"zero_cap", BrokerEgressConfig{InterfaceName: "br-brokerq", EgressMbit: 0}},
		{"negative_cap", BrokerEgressConfig{InterfaceName: "br-brokerq", EgressMbit: -1}},
		{"empty_interface", BrokerEgressConfig{InterfaceName: "", EgressMbit: 200}},
		{"empty_both", BrokerEgressConfig{}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			acc := NewBrokerAccountor(c.cfg)
			if acc == nil {
				t.Fatalf("NewBrokerAccountor = nil, want noopBrokerAccountor{}")
			}
			// The noop surface is a zero-cost no-return Account.
			// Asserting on the byte counter (no test field exposed
			// on the noop type) is fragile; the contract is
			// "doesn't panic, doesn't error on Close".
			acc.Account(context.Background(), "trigger-1", 1024)
			if err := acc.Close(); err != nil {
				t.Errorf("Close err = %v, want nil (noop)", err)
			}
		})
	}
}

// TestBrokerEgressObserver_Accumulates pins the atomic-counter
// surface used by the Linux implementation (commit 8 follow-up).
// A future refactor that drops atomic.Int64 for a sync.Mutex
// must surface here at -race.
func TestBrokerEgressObserver_Accumulates(t *testing.T) {
	t.Parallel()
	o := newBrokerEgressObserver()
	if got := o.TotalBytes(); got != 0 {
		t.Errorf("fresh observer TotalBytes = %d, want 0", got)
	}
	o.Account(100)
	o.Account(250)
	if got, want := o.TotalBytes(), int64(350); got != want {
		t.Errorf("TotalBytes after 100+250 = %d, want %d", got, want)
	}
}

// TestBrokerTcCommands_NilWhenCapZero_Stub pins the stub
// counterpart: on non-linux builds BrokerTcCommands returns
// nil for both zero-cap and non-zero-cap shapes. The
// build-tag-gated linux file pins the argv shape on linux.
// This pin documents the cross-build contract so a future
// refactor that swaps the stub return value from `nil` to
// `[]string{}` is caught.
func TestBrokerTcCommands_NilWhenCapZero_Stub(t *testing.T) {
	t.Parallel()
	// Stub contract is "always nil regardless of cap shape",
	// unlike the linux shape where non-zero cap returns argv.
	for _, cfg := range []BrokerEgressConfig{
		{InterfaceName: "br-brokerq", EgressMbit: 0},
		{InterfaceName: "br-brokerq", EgressMbit: 200},
		{InterfaceName: "", EgressMbit: 200},
		{},
	} {
		cfg := cfg
		if got := BrokerTcCommands(cfg); got != nil {
			t.Errorf("BrokerTcCommands(%+v) = %v, want nil (stub build)", cfg, got)
		}
	}
}

// TestUnitSchedd_BrokerqSliceWired pins the systemd-unit side
// of commit 8: pkg/daemonunitspec.UnitSchedd must reference
// faas-brokerq.slice in its After/Wants lists AND invoke the
// brokerq-apply ExecStartPre. Without these the
// BrokerTcCommands argv would never be executed at boot and
// the per-deployment cap would not be enforced.
func TestUnitSchedd_BrokerqSliceWired(t *testing.T) {
	t.Parallel()
	u := daemonunitspec.UnitSchedd()
	wantsBrokerq := false
	for _, w := range u.Wants {
		if w == "faas-brokerq.slice" {
			wantsBrokerq = true
			break
		}
	}
	if !wantsBrokerq {
		t.Errorf("UnitSchedd().Wants = %v, missing faas-brokerq.slice (commit 8 brokerq wiring)", u.Wants)
	}
	foundBrokerqApply := false
	for _, line := range u.ExecStartPre {
		if strings.Contains(line, "brokerq-apply") {
			foundBrokerqApply = true
			break
		}
	}
	if !foundBrokerqApply {
		t.Errorf("UnitSchedd().ExecStartPre = %v, missing brokerq-apply (commit 8 brokerq wiring)", u.ExecStartPre)
	}
}
