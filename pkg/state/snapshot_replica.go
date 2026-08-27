package state

import "context"

// SnapshotReplicaState is the durable state of one snapshot's local
// prepositioning job on one compute node. The snapshot itself remains
// authoritative in the shared storage backend; this table is only a
// resumable cache-warming index.
type SnapshotReplicaState string

const (
	SnapshotReplicaPending SnapshotReplicaState = "pending"
	SnapshotReplicaSyncing SnapshotReplicaState = "syncing"
	SnapshotReplicaReady   SnapshotReplicaState = "ready"
	SnapshotReplicaFailed  SnapshotReplicaState = "failed"
)

// SnapshotReplicaJob is the complete input needed by a node-local fan-out
// worker. VMStateStorageKey is derived from the immutable snapshot tier so
// both restore blobs are prepositioned together.
type SnapshotReplicaJob struct {
	SnapshotID        string
	DeploymentID      string
	StorageKey        string
	VMStateStorageKey string
	Tier              string
	NodeID            string
	Region            string
	Attempts          int
}

// SnapshotOriginStore records the node/locality that produced a snapshot.
// Older rows may have no origin; the fan-out reconciler treats those rows as
// eligible for every active node so a rollout never strands legacy snapshots.
type SnapshotOriginStore interface {
	RecordSnapshotOrigin(ctx context.Context, snapshotID, nodeID string) error
}

// SnapshotReplicaStore is intentionally optional instead of being folded into
// Store. That keeps existing test seams and external state implementations
// source-compatible while PgStore and MemStore gain the same production
// capability. A worker only starts when the concrete store implements this
// interface.
type SnapshotReplicaStore interface {
	// EnqueueSnapshotReplicasForNode consumes the global snapshot fan-out event
	// cursor for this node and creates idempotent warming jobs. Implementations
	// must be safe to call on every worker tick. The production implementation
	// consumes only events newer than the node cursor; in-memory/test
	// implementations may retain a full-snapshot fallback.
	EnqueueSnapshotReplicasForNode(ctx context.Context, nodeID string) (int, error)
	// ClaimSnapshotReplica atomically leases one pending/retryable job for the
	// node. ErrNotFound means the queue is empty.
	ClaimSnapshotReplica(ctx context.Context, nodeID string) (SnapshotReplicaJob, error)
	MarkSnapshotReplicaReady(ctx context.Context, snapshotID, nodeID string) error
	MarkSnapshotReplicaFailed(ctx context.Context, snapshotID, nodeID string, cause error) error
	// ReadySnapshotReplicaNodes returns nodes whose local cache has a complete
	// copy of the snapshot's restore blobs.
	ReadySnapshotReplicaNodes(ctx context.Context, snapshotID string) ([]string, error)
}
