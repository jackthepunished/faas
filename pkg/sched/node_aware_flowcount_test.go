package sched

import (
	"context"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/state"
)

type nodeAwareFlowFallback struct {
	warmed int
	count  int64
}

func (f *nodeAwareFlowFallback) Warm(_ context.Context, instances []state.Instance) error {
	f.warmed = len(instances)
	return nil
}

func (f *nodeAwareFlowFallback) Open(_ context.Context, _ string) (int64, error) {
	return f.count, nil
}

func TestNodeAwareFlowCounterPrefersFreshRemoteTelemetry(t *testing.T) {
	cache := NewNodeTelemetryCache()
	now := time.Unix(300, 0)
	remote := int64(4)
	cache.Replace("node-a", now, now, []NodeTelemetry{{InstanceID: "vm-1", OpenConns: remote}})
	fallback := &nodeAwareFlowFallback{count: 99}
	counter := NewNodeAwareFlowCounter(cache, fallback)
	counter.now = func() time.Time { return now }

	got, err := counter.Open(context.Background(), "vm-1")
	if err != nil || got != remote {
		t.Fatalf("Open = (%d, %v), want (%d, nil)", got, err, remote)
	}
}

func TestNodeAwareFlowCounterFallsBackWhenRemoteIsMissingOrStale(t *testing.T) {
	cache := NewNodeTelemetryCache()
	now := time.Unix(400, 0)
	remote := int64(4)
	cache.Replace("node-a", now, now, []NodeTelemetry{{InstanceID: "vm-1", OpenConns: remote}})
	fallback := &nodeAwareFlowFallback{count: 7}
	counter := NewNodeAwareFlowCounter(cache, fallback)
	counter.now = func() time.Time { return now.Add(TelemetryFreshness + time.Nanosecond) }

	got, err := counter.Open(context.Background(), "vm-1")
	if err != nil || got != fallback.count {
		t.Fatalf("stale Open = (%d, %v), want (%d, nil)", got, err, fallback.count)
	}
	if got, err := counter.Open(context.Background(), "unknown"); err != nil || got != fallback.count {
		t.Fatalf("missing Open = (%d, %v), want (%d, nil)", got, err, fallback.count)
	}
}

func TestNodeAwareFlowCounterWarmForwardsLocalReader(t *testing.T) {
	fallback := &nodeAwareFlowFallback{}
	counter := NewNodeAwareFlowCounter(nil, fallback)
	instances := []state.Instance{{ID: "vm-1"}, {ID: "vm-2"}}
	if err := counter.Warm(context.Background(), instances); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	if fallback.warmed != len(instances) {
		t.Fatalf("fallback warmed %d instances, want %d", fallback.warmed, len(instances))
	}
}
