package vmmdgrpc

import (
	"context"
	"testing"

	"github.com/onebox-faas/faas/pkg/state"
)

type hostIPVMM struct {
	vmmStubBase
	hosts map[string]string
}

func (v hostIPVMM) SnapshotLiveHostIPs() map[string]string { return v.hosts }

type openConnsCounter struct {
	warmed int
	counts map[string]int64
}

func (c *openConnsCounter) Warm(_ context.Context, instances []state.Instance) error {
	c.warmed = len(instances)
	return nil
}

func (c *openConnsCounter) Open(_ context.Context, instanceID string) (int64, error) {
	return c.counts[instanceID], nil
}

func TestServerOpenConnsUsesComputeHostSnapshot(t *testing.T) {
	counter := &openConnsCounter{counts: map[string]int64{"vm-1": 3, "vm-2": 1}}
	server := &Server{
		vmm:         hostIPVMM{hosts: map[string]string{"vm-1": "10.100.0.2", "vm-2": "10.100.0.3"}},
		flowCounter: counter,
	}

	got := server.openConns(context.Background())
	if counter.warmed != 2 {
		t.Fatalf("Warm received %d instances, want 2", counter.warmed)
	}
	if got["vm-1"] != 3 || got["vm-2"] != 1 {
		t.Fatalf("open-conns = %#v, want vm-1=3 vm-2=1", got)
	}
}
