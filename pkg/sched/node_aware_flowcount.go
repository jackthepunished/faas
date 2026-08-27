package sched

import (
	"context"
	"time"

	"github.com/onebox-faas/faas/pkg/state"
)

// NodeAwareFlowCounter prefers the compute-side conntrack count transported
// in NodeTelemetryCache and falls back to the local reader for single-box
// deployments or during a telemetry gap. This keeps the reaper's existing
// fail-open contract while making the split-box decision based on the node
// that actually owns the VM.
type NodeAwareFlowCounter struct {
	telemetry *NodeTelemetryCache
	fallback  FlowCounter
	now       func() time.Time
}

// NewNodeAwareFlowCounter composes the remote telemetry cache with the
// existing local FlowCounter. A nil fallback is valid and returns zero on a
// cache miss, matching the legacy no-op behavior.
func NewNodeAwareFlowCounter(telemetry *NodeTelemetryCache, fallback FlowCounter) *NodeAwareFlowCounter {
	return &NodeAwareFlowCounter{telemetry: telemetry, fallback: fallback, now: time.Now}
}

// Warm forwards the local reader's bulk snapshot when it supports Warm. The
// remote cache is populated by ReportCapacity and does not need warming here.
func (c *NodeAwareFlowCounter) Warm(ctx context.Context, instances []state.Instance) error {
	if c == nil || c.fallback == nil {
		return nil
	}
	warmer, ok := c.fallback.(interface {
		Warm(context.Context, []state.Instance) error
	})
	if !ok {
		return nil
	}
	return warmer.Warm(ctx, instances)
}

// Open returns remote compute telemetry when fresh, then delegates to the
// local fallback. Both paths retain the reaper's fail-open behavior.
func (c *NodeAwareFlowCounter) Open(ctx context.Context, instanceID string) (int64, error) {
	if c == nil {
		return 0, nil
	}
	now := time.Now()
	if c.now != nil {
		now = c.now()
	}
	if value, ok := c.telemetry.LookupOpenConns(instanceID, now); ok {
		return value, nil
	}
	if c.fallback != nil {
		return c.fallback.Open(ctx, instanceID)
	}
	return 0, nil
}
