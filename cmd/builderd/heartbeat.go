package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/onebox-faas/faas/pkg/state"
)

const defaultBuilderHeartbeatInterval = 15 * time.Second

// builderHeartbeatStore is the small part of state.Store needed by the
// builderd liveness publisher. Keeping this seam narrow makes the writer
// testable without coupling it to the entire persistence interface.
type builderHeartbeatStore interface {
	ComputeNodeByName(context.Context, string) (state.ComputeNode, error)
	AppendComputeNodeHeartbeat(context.Context, string, time.Time, time.Time, string) error
}

// builderHeartbeatNodeName resolves the modern node identity first and keeps
// the legacy BuilderNodeID fallback working for single-box configurations.
func builderHeartbeatNodeName(cfg *Config) string {
	if cfg == nil {
		return ""
	}
	if cfg.NodeName != "" {
		return cfg.NodeName
	}
	return cfg.BuilderNodeID
}

// publishBuilderHeartbeat records one builder_tick row. A missing compute
// node is deliberately retryable: vmmd owns compute-node self-registration and
// builderd may start before vmmd has completed that registration on a fresh
// bare-metal box.
func publishBuilderHeartbeat(ctx context.Context, store builderHeartbeatStore, nodeName string, now time.Time) error {
	if nodeName == "" {
		return nil
	}
	node, err := store.ComputeNodeByName(ctx, nodeName)
	if err != nil {
		return err
	}
	return store.AppendComputeNodeHeartbeat(ctx, node.ID, now, now, "builder_tick")
}

// builderHeartbeatLoop publishes a durable heartbeat independently of the
// build queue. An idle builder is still healthy, and the operator view must be
// able to distinguish "no builds" from "builderd is gone".
func builderHeartbeatLoop(ctx context.Context, store builderHeartbeatStore, nodeName string, interval time.Duration, log *slog.Logger) {
	if interval <= 0 {
		interval = defaultBuilderHeartbeatInterval
	}
	if log == nil {
		log = slog.Default()
	}

	publish := func() {
		if err := publishBuilderHeartbeat(ctx, store, nodeName, time.Now().UTC()); err != nil {
			log.Warn("builderd: publish heartbeat", "node", nodeName, "err", err)
		}
	}
	// Publish immediately so a newly started builder is visible without
	// waiting for the first cadence tick.
	publish()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			publish()
		}
	}
}
