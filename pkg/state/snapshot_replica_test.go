package state

import (
	"context"
	"errors"
	"testing"
)

func TestMemStoreSnapshotReplicaLifecycle(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	region := DefaultLocalityLabel
	second := ComputeNode{
		ID: "node-2", Name: "compute-2", TargetURL: "unix:///run/faas/compute-2.sock",
		AdmissionCeilingMB: 4096, VCPUBudget: 16, Active: true,
		Region: &region,
	}
	if _, err := m.CreateComputeNode(ctx, second); err != nil {
		t.Fatalf("CreateComputeNode: %v", err)
	}
	snap, err := m.CreateSnapshot(ctx, Snapshot{
		ID: "snap-1", DeploymentID: "dep-1", FCVersion: "fc-1", StorageKey: SnapMemKey("dep-1"),
	})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	local, err := m.ComputeNodeByName(ctx, DefaultLocalNodeName)
	if err != nil {
		t.Fatalf("ComputeNodeByName: %v", err)
	}
	if err := m.RecordSnapshotOrigin(ctx, snap.ID, local.ID); err != nil {
		t.Fatalf("RecordSnapshotOrigin: %v", err)
	}
	added, err := m.EnqueueSnapshotReplicasForNode(ctx, second.ID)
	if err != nil {
		t.Fatalf("EnqueueSnapshotReplicasForNode: %v", err)
	}
	if added != 1 {
		t.Fatalf("enqueued = %d, want 1", added)
	}
	job, err := m.ClaimSnapshotReplica(ctx, second.ID)
	if err != nil {
		t.Fatalf("ClaimSnapshotReplica: %v", err)
	}
	if job.SnapshotID != snap.ID || job.VMStateStorageKey != SnapVMStateKey("dep-1") {
		t.Fatalf("job = %+v", job)
	}
	if err := m.MarkSnapshotReplicaReady(ctx, snap.ID, second.ID); err != nil {
		t.Fatalf("MarkSnapshotReplicaReady: %v", err)
	}
	ready, err := m.ReadySnapshotReplicaNodes(ctx, snap.ID)
	if err != nil {
		t.Fatalf("ReadySnapshotReplicaNodes: %v", err)
	}
	if len(ready) != 1 || ready[0] != second.ID {
		t.Fatalf("ready nodes = %v, want [%s]", ready, second.ID)
	}
	if _, err := m.ClaimSnapshotReplica(ctx, second.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second claim err = %v, want ErrNotFound", err)
	}
}
