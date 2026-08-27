package main

import (
	"context"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/state"
)

func TestBuilderHeartbeatNodeNamePrefersNodeName(t *testing.T) {
	if got := builderHeartbeatNodeName(&Config{NodeName: "fsn-2", BuilderNodeID: "legacy"}); got != "fsn-2" {
		t.Fatalf("node name = %q, want fsn-2", got)
	}
	if got := builderHeartbeatNodeName(&Config{BuilderNodeID: "default-local"}); got != "default-local" {
		t.Fatalf("legacy node name = %q, want default-local", got)
	}
}

func TestPublishBuilderHeartbeatAppendsBuilderTick(t *testing.T) {
	store := state.NewMemStore()
	now := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)

	if err := publishBuilderHeartbeat(context.Background(), store, state.DefaultLocalNodeName, now); err != nil {
		t.Fatalf("publishBuilderHeartbeat: %v", err)
	}
	node, err := store.ComputeNodeByName(context.Background(), state.DefaultLocalNodeName)
	if err != nil {
		t.Fatalf("ComputeNodeByName: %v", err)
	}
	rows, err := store.ListComputeNodeHeartbeats(context.Background(), node.ID, time.Time{}, 10)
	if err != nil {
		t.Fatalf("ListComputeNodeHeartbeats: %v", err)
	}
	if len(rows) != 1 || rows[0].Source != "builder_tick" {
		t.Fatalf("heartbeats = %#v, want one builder_tick row", rows)
	}
	if !rows[0].ReceivedAt.Equal(now) {
		t.Fatalf("received_at = %s, want %s", rows[0].ReceivedAt, now)
	}
}

func TestBuilderHeartbeatLoopStopsOnCancel(t *testing.T) {
	store := state.NewMemStore()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		builderHeartbeatLoop(ctx, store, state.DefaultLocalNodeName, time.Hour, nil)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for {
		node, err := store.ComputeNodeByName(context.Background(), state.DefaultLocalNodeName)
		if err != nil {
			t.Fatalf("ComputeNodeByName: %v", err)
		}
		rows, err := store.ListComputeNodeHeartbeats(context.Background(), node.ID, time.Time{}, 10)
		if err != nil {
			t.Fatalf("ListComputeNodeHeartbeats: %v", err)
		}
		if len(rows) == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("heartbeat was not published")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("heartbeat loop did not stop")
	}
}
