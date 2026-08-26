package state

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const snapshotReplicaRetryDelay = 5 * time.Second

// EnqueueSnapshotReplicasForNode is the reconciliation half of snapshot
// fan-out. It is safe to run at every worker tick and also repairs jobs missed
// while a node or schedd was offline.
func (s *PgStore) EnqueueSnapshotReplicasForNode(ctx context.Context, nodeID string) (int, error) {
	if nodeID == "" {
		return 0, errors.New("state: enqueue snapshot replicas: node_id required")
	}
	tag, err := s.pool.Exec(ctx, `
		insert into snapshot_replicas (snapshot_id, node_id, region)
		select sn.id, cn.id, coalesce(cn.region, '')
		from snapshots sn
		cross join compute_nodes cn
		left join snapshot_origins so on so.snapshot_id = sn.id
		where cn.id = $1
		  and cn.active = true
		  and sn.stale = false
		  and sn.storage_key <> ''
		  and (so.snapshot_id is null or so.region = '' or so.region = coalesce(cn.region, ''))
		  and (so.snapshot_id is null or so.node_id is null or so.node_id <> cn.id)
		on conflict (snapshot_id, node_id) do nothing`, nodeID)
	if err != nil {
		return 0, fmt.Errorf("state: enqueue snapshot replicas for %s: %w", nodeID, err)
	}
	return int(tag.RowsAffected()), nil
}

// RecordSnapshotOrigin lets the reconciler restrict fan-out to the producer's
// region without widening the hot snapshots row or making old rows invalid.
func (s *PgStore) RecordSnapshotOrigin(ctx context.Context, snapshotID, nodeID string) error {
	if snapshotID == "" || nodeID == "" {
		return errors.New("state: record snapshot origin: snapshot_id and node_id required")
	}
	tag, err := s.pool.Exec(ctx, `
		insert into snapshot_origins (snapshot_id, node_id, region)
		select $1, cn.id, coalesce(cn.region, '')
		from compute_nodes cn
		where cn.id = $2
		on conflict (snapshot_id) do update
		set node_id = excluded.node_id, region = excluded.region`, snapshotID, nodeID)
	if err != nil {
		return fmt.Errorf("state: record snapshot origin: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ClaimSnapshotReplica leases one job with row locking. A stale syncing lease
// is reclaimable after five minutes so a crashed node-local worker cannot
// strand a snapshot forever.
func (s *PgStore) ClaimSnapshotReplica(ctx context.Context, nodeID string) (SnapshotReplicaJob, error) {
	if nodeID == "" {
		return SnapshotReplicaJob{}, errors.New("state: claim snapshot replica: node_id required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SnapshotReplicaJob{}, fmt.Errorf("state: claim snapshot replica begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var job SnapshotReplicaJob
	row := tx.QueryRow(ctx, `
		select r.snapshot_id::text,
		       sn.deployment_id::text,
		       sn.storage_key,
		       case when coalesce(sn.tier, 'init') = 'warm'
		            then 'snap/' || sn.deployment_id::text || '/warm/vmstate'
		            else 'snap/' || sn.deployment_id::text || '/vmstate'
		       end,
		       coalesce(sn.tier, 'init'),
		       r.node_id::text,
		       coalesce(cn.region, ''),
		       r.attempts
		from snapshot_replicas r
		join snapshots sn on sn.id = r.snapshot_id
		join compute_nodes cn on cn.id = r.node_id
		where r.node_id = $1
		  and sn.stale = false
		  and (
				r.state in ('pending', 'failed')
				or (r.state = 'syncing' and r.updated_at < now() - interval '5 minutes')
			  )
		  and (r.next_attempt_at is null or r.next_attempt_at <= now())
		order by r.created_at, r.snapshot_id
		for update of r skip locked
		limit 1`, nodeID)
	if err := row.Scan(&job.SnapshotID, &job.DeploymentID, &job.StorageKey,
		&job.VMStateStorageKey, &job.Tier, &job.NodeID, &job.Region, &job.Attempts); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SnapshotReplicaJob{}, ErrNotFound
		}
		return SnapshotReplicaJob{}, fmt.Errorf("state: claim snapshot replica scan: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		update snapshot_replicas
		set state = 'syncing', attempts = attempts + 1,
		    updated_at = now(), next_attempt_at = null, last_error = null
		where snapshot_id = $1 and node_id = $2`, job.SnapshotID, job.NodeID); err != nil {
		return SnapshotReplicaJob{}, fmt.Errorf("state: claim snapshot replica update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return SnapshotReplicaJob{}, fmt.Errorf("state: claim snapshot replica commit: %w", err)
	}
	job.Attempts++
	return job, nil
}

func (s *PgStore) MarkSnapshotReplicaReady(ctx context.Context, snapshotID, nodeID string) error {
	return s.markSnapshotReplica(ctx, snapshotID, nodeID, string(SnapshotReplicaReady), "", time.Time{})
}

func (s *PgStore) MarkSnapshotReplicaFailed(ctx context.Context, snapshotID, nodeID string, cause error) error {
	message := "snapshot replica failed"
	if cause != nil {
		message = cause.Error()
	}
	message = strings.TrimSpace(message)
	if len(message) > 2048 {
		message = message[:2048]
	}
	return s.markSnapshotReplica(ctx, snapshotID, nodeID, string(SnapshotReplicaFailed), message, time.Now().Add(snapshotReplicaRetryDelay))
}

func (s *PgStore) markSnapshotReplica(ctx context.Context, snapshotID, nodeID, status, message string, retryAt time.Time) error {
	if snapshotID == "" || nodeID == "" {
		return errors.New("state: mark snapshot replica: snapshot_id and node_id required")
	}
	var tag interface{ RowsAffected() int64 }
	var err error
	if retryAt.IsZero() {
		tag, err = s.pool.Exec(ctx, `
			update snapshot_replicas
			set state = $3, last_error = null, ready_at = now(), updated_at = now(), next_attempt_at = null
			where snapshot_id = $1 and node_id = $2`, snapshotID, nodeID, status)
	} else {
		tag, err = s.pool.Exec(ctx, `
			update snapshot_replicas
			set state = $3, last_error = $4, ready_at = null, updated_at = now(), next_attempt_at = $5
			where snapshot_id = $1 and node_id = $2`, snapshotID, nodeID, status, message, retryAt)
	}
	if err != nil {
		return fmt.Errorf("state: mark snapshot replica %s: %w", status, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PgStore) ReadySnapshotReplicaNodes(ctx context.Context, snapshotID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		select node_id::text
		from snapshot_replicas
		where snapshot_id = $1 and state = 'ready'
		order by node_id::text`, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("state: ready snapshot replica nodes: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var nodeID string
		if err := rows.Scan(&nodeID); err != nil {
			return nil, fmt.Errorf("state: ready snapshot replica node scan: %w", err)
		}
		out = append(out, nodeID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: ready snapshot replica nodes rows: %w", err)
	}
	return out, nil
}
