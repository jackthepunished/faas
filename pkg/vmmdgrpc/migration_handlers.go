// migration_handlers.go — Tier A5 (ADR-066) four-phase
// cross-node live-instance migration handlers on the vmmd
// side.
//
// IMPORTANT — the dying vmmd does NOT keep the VM paused
// during the migration window. pkg/fcvm.Park destroys the VM
// as part of its contract (see pkg/fcvm/manager.go:1207: "Park
// snapshots a running instance then destroys it, freeing all
// resident RAM"). A live migration therefore has a one-shot
// blip on the customer side: Phase 1 → VM gone from dying
// vmmd, snapshot in canonical storage. Phase 3 → new owner
// restores the snapshot (cold-boot from snapshot, ~350 ms).
// The migration is a "live migration" in the sense that the
// instance survives (state preserved on disk) but the VM
// process itself is destroyed and recreated on the new owner
// — there is no pause-and-keep-running model because that
// would require VM-level live migration primitives that
// Firecracker does not expose.
//
// The four phases map to the gRPC handlers as:
//
//   Phase 1  PrepareLiveMigration     — dying vmmd, s.vmm.Park
//                                       + lease mint + tracker put.
//   Phase 3  AdoptMigratedInstance    — new owner vmmd, lease
//                                       validate (Restore is
//                                       driven by the schedd
//                                       orchestrator via the
//                                       typed wrapper in
//                                       pkg/sched/vmmclient.go).
//   Phase 4  CancelLiveMigration      — dying vmmd, lease delete.
//                                       VM is already gone (Park
//                                       destroyed it); the cancel
//                                       is a soft "I changed my
//                                       mind" marker for telemetry.
//   Phase 5  AcknowledgeMigration     — dying vmmd, lease delete.
//                                       Idempotent with Phase 4
//                                       (a Phase 5 after a Phase 4
//                                       sees no lease and returns
//                                       success).
//
// The lease is the dying vmmd's authority. It is minted at
// Phase 1 and consulted only at Phase 3 (new owner proves
// the token before Restore). Phase 4 / 5 just delete the
// tracker entry. On lease expiry (api.MigrateLiveLeaseSeconds)
// the entry is dropped and the snapshot stays in storage
// until the per-vmmd snapshot-drift sweep reaps it.

package vmmdgrpc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"

	vmmdpb "github.com/onebox-faas/faas/api/proto/onebox/faas/vmmd/v1"
	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/fcvm"
	"github.com/onebox-faas/faas/pkg/grpcerr"
)

// activeMigration is the in-memory tracker entry for a
// completed Phase 1. The VM is already destroyed (Park's
// contract); the entry exists so the new owner can prove
// the lease token at Phase 3, and so the lease-expiry
// loop can sweep stale leases.
type activeMigration struct {
	instanceID     string
	leaseToken     string
	createdAt      time.Time
	leaseExpiresAt time.Time
	memKey         string
	vmstateKey     string
}

// migrationTracker is the per-vmmd in-memory map of
// completed-but-not-yet-acked-or-cancelled Phase 1 results.
// The lease-expiry loop is the only background consumer.
type migrationTracker struct {
	mu    sync.Mutex
	state map[string]*activeMigration
}

func newMigrationTracker() *migrationTracker {
	return &migrationTracker{state: map[string]*activeMigration{}}
}

// put inserts a new entry. errAlreadyActive if the
// instance already has an active migration.
func (t *migrationTracker) put(m *activeMigration) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.state[m.instanceID]; ok {
		return errAlreadyActive{instanceID: m.instanceID}
	}
	t.state[m.instanceID] = m
	return nil
}

// get fetches by instanceID + leaseToken. errNoLease on
// unknown instance, errLeaseMismatch on token mismatch.
func (t *migrationTracker) get(instanceID, leaseToken string) (*activeMigration, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	m, ok := t.state[instanceID]
	if !ok {
		return nil, errNoLease{instanceID: instanceID}
	}
	if m.leaseToken != leaseToken {
		return nil, errLeaseMismatch{instanceID: instanceID}
	}
	return m, nil
}

// delete removes an entry. Idempotent.
func (t *migrationTracker) delete(instanceID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.state, instanceID)
}

// listExpired returns the entries whose lease has expired
// (LeaseExpiresAt < now).
func (t *migrationTracker) listExpired(now time.Time) []*activeMigration {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []*activeMigration
	for _, m := range t.state {
		if now.After(m.leaseExpiresAt) {
			out = append(out, m)
		}
	}
	return out
}

// errAlreadyActive / errNoLease / errLeaseMismatch are
// tracker sentinel errors, mapped to gRPC codes by the
// handlers below.
type errAlreadyActive struct{ instanceID string }

func (e errAlreadyActive) Error() string {
	return "vmmd: migration already active for instance " + e.instanceID
}

type errNoLease struct{ instanceID string }

func (e errNoLease) Error() string {
	return "vmmd: no active migration for instance " + e.instanceID
}

type errLeaseMismatch struct{ instanceID string }

func (e errLeaseMismatch) Error() string {
	return "vmmd: lease token mismatch for instance " + e.instanceID
}

// mintLeaseToken mints a 128-bit hex token. crypto/rand is
// the source; uuid.NewString is the fallback if the OS RNG
// fails (degenerate but well-defined).
func mintLeaseToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return uuid.NewString()
	}
	return hex.EncodeToString(b[:])
}

// PrepareLiveMigration — Phase 1.
//
// Wire errors:
//
//	codes.InvalidArgument   missing instanceID or snapshot_storage_key
//	codes.AlreadyExists     duplicate Phase 1 for this instance
//	codes.Internal          Park failed (FC uAPI / storage); the VM
//	                        is destroyed by Park's failure path so
//	                        invariant §6.2-4 still holds.
func (s *Server) PrepareLiveMigration(ctx context.Context, req *vmmdpb.PrepareLiveMigrationRequest) (*vmmdpb.PrepareLiveMigrationResponse, error) {
	const op = "PrepareLiveMigration"
	start := time.Now()
	if req.GetInstanceId() == "" || req.GetSnapshotStorageKey() == "" {
		err := api.NewProblem(int(codes.InvalidArgument), api.CodeValidation,
			"Missing fields",
			"instance_id and snapshot_storage_key are required").WithDocs("https://docs/vmmd#prepare")
		s.ops.Observe(op, time.Since(start), err)
		return nil, grpcerr.ToStatus(err)
	}

	memKey := req.GetSnapshotStorageKey()
	// vmstateKey is the canonical sibling under the same
	// namespace. Tier A5 uses "snap/<key-base>/{mem,vmstate}":
	// strip the trailing "/mem" and append "/vmstate".
	// Fallback "-vmstate" is defence-in-depth (the orchestrator
	// always emits the slash-suffixed form).
	vmstateKey := memKey
	if len(vmstateKey) >= 4 && vmstateKey[len(vmstateKey)-4:] == "/mem" {
		vmstateKey = vmstateKey[:len(vmstateKey)-4] + "/vmstate"
	} else {
		vmstateKey = vmstateKey + "-vmstate"
	}

	// Park snapshots the VM to canonical storage and destroys
	// the VM as part of its contract (pkg/fcvm/manager.go:1207).
	// On success the snapshot lives at memKey / vmstateKey;
	// the dying vmmd's live-map entry is gone.
	_, err := s.vmm.Park(ctx, req.GetInstanceId(), fcvm.SnapshotSpec{
		StageMemPath:      "",
		VMStatePath:       "",
		StorageKey:        memKey,
		VMStateStorageKey: vmstateKey,
	})
	if err != nil {
		// Park's failure path runs Destroy, so the live map
		// is consistent (invariant §6.2-4/5). The orchestrator's
		// Phase 4 is therefore a no-op on the dying vmmd.
		s.ops.Observe(op, time.Since(start), err)
		return nil, grpcerr.ToStatus(toProblem(err))
	}

	lease := mintLeaseToken()
	m := &activeMigration{
		instanceID:     req.GetInstanceId(),
		leaseToken:     lease,
		createdAt:      start,
		leaseExpiresAt: start.Add(time.Duration(api.MigrateLiveLeaseSeconds) * time.Second),
		memKey:         memKey,
		vmstateKey:     vmstateKey,
	}
	if err := s.migrations.put(m); err != nil {
		// Duplicate Phase 1 — the lease for this instance
		// already exists; the previous lease owns the
		// canonical snapshot. Surface codes.AlreadyExists
		// so the orchestrator can distinguish from a fresh
		// Park failure.
		err2 := api.NewProblem(int(codes.AlreadyExists), api.CodeConflict,
			"Migration already active", err.Error()).WithDocs("https://docs/vmmd#prepare")
		s.ops.Observe(op, time.Since(start), err2)
		return nil, grpcerr.ToStatus(err2)
	}
	s.ops.Observe(op, time.Since(start), nil)
	return &vmmdpb.PrepareLiveMigrationResponse{
		MemStorageKey:     memKey,
		VmstateStorageKey: vmstateKey,
		LeaseToken:        lease,
	}, nil
}

// AdoptMigratedInstance — Phase 3.
//
// The new owner vmmd validates the lease (dying vmmd's
// authority) and signals readiness to receive the snapshot.
// The actual Restore is driven by the schedd orchestrator
// via pkg/sched.LiveMigrationAdopt (typed wrapper), not
// through this handler — the handler exists for symmetry,
// future operator-triggered migration flows, and the wire-
// level feature surface.
//
// Wire errors:
//
//	codes.InvalidArgument   missing required fields
//	codes.NotFound          lease is gone (Phase 4/5 ran, or
//	                        lease expired)
//	codes.PermissionDenied  lease token mismatch
func (s *Server) AdoptMigratedInstance(ctx context.Context, req *vmmdpb.AdoptMigratedInstanceRequest) (*vmmdpb.AdoptMigratedInstanceResponse, error) {
	const op = "AdoptMigratedInstance"
	start := time.Now()
	if req.GetInstanceId() == "" || req.GetMemStorageKey() == "" ||
		req.GetVmstateStorageKey() == "" || req.GetLeaseToken() == "" {
		err := api.NewProblem(int(codes.InvalidArgument), api.CodeValidation,
			"Missing fields",
			"instance_id, mem_storage_key, vmstate_storage_key, and lease_token are required").
			WithDocs("https://docs/vmmd#adopt")
		s.ops.Observe(op, time.Since(start), err)
		return nil, grpcerr.ToStatus(err)
	}
	if _, err := s.migrations.get(req.GetInstanceId(), req.GetLeaseToken()); err != nil {
		var code int
		var apiCode string
		switch err.(type) {
		case errLeaseMismatch:
			code, apiCode = int(codes.PermissionDenied), api.CodeUnauthorized
		default:
			code, apiCode = int(codes.NotFound), api.CodeNotFound
		}
		err2 := api.NewProblem(code, apiCode, "Lease lookup failed", err.Error()).
			WithDocs("https://docs/vmmd#adopt")
		s.ops.Observe(op, time.Since(start), err2)
		return nil, grpcerr.ToStatus(err2)
	}
	s.ops.Observe(op, time.Since(start), nil)
	return &vmmdpb.AdoptMigratedInstanceResponse{}, nil
}

// AcknowledgeMigration — Phase 5.
//
// The new owner vmmd's schedd tells the dying vmmd "Phase 3
// committed; tear down the lease". The VM is already gone
// (Park destroyed it at Phase 1), so this handler is just a
// tracker delete. Idempotent on a stale Phase 5.
//
// Wire errors:
//
//	codes.InvalidArgument   missing fields
//	(all other errors absorbed: idempotent)
func (s *Server) AcknowledgeMigration(ctx context.Context, req *vmmdpb.AcknowledgeMigrationRequest) (*vmmdpb.AcknowledgeMigrationResponse, error) {
	const op = "AcknowledgeMigration"
	start := time.Now()
	if req.GetInstanceId() == "" || req.GetLeaseToken() == "" {
		err := api.NewProblem(int(codes.InvalidArgument), api.CodeValidation,
			"Missing fields",
			"instance_id and lease_token are required").WithDocs("https://docs/vmmd#ack")
		s.ops.Observe(op, time.Since(start), err)
		return nil, grpcerr.ToStatus(err)
	}
	if _, err := s.migrations.get(req.GetInstanceId(), req.GetLeaseToken()); err != nil {
		// Stale ack — lease already cleared. Idempotent
		// success.
		s.ops.Observe(op, time.Since(start), nil)
		return &vmmdpb.AcknowledgeMigrationResponse{}, nil
	}
	s.migrations.delete(req.GetInstanceId())
	s.ops.Observe(op, time.Since(start), nil)
	return &vmmdpb.AcknowledgeMigrationResponse{}, nil
}

// CancelLiveMigration — Phase 4.
//
// The new owner vmmd's schedd tells the dying vmmd "abort —
// don't commit Phase 3". The VM is already gone, so this
// handler is just a tracker delete. The canonical snapshot
// the dying vmmd wrote at Phase 1 stays in storage until
// the per-vmmd snapshot-drift sweep reaps it; the snapshot
// may re-serve a future cold boot on this instance, identical
// to a normal Park output.
//
// Wire errors:
//
//	codes.InvalidArgument   missing fields
//	(all other errors absorbed: idempotent)
func (s *Server) CancelLiveMigration(ctx context.Context, req *vmmdpb.CancelLiveMigrationRequest) (*vmmdpb.CancelLiveMigrationResponse, error) {
	const op = "CancelLiveMigration"
	start := time.Now()
	if req.GetInstanceId() == "" || req.GetLeaseToken() == "" {
		err := api.NewProblem(int(codes.InvalidArgument), api.CodeValidation,
			"Missing fields",
			"instance_id and lease_token are required").WithDocs("https://docs/vmmd#cancel")
		s.ops.Observe(op, time.Since(start), err)
		return nil, grpcerr.ToStatus(err)
	}
	if _, err := s.migrations.get(req.GetInstanceId(), req.GetLeaseToken()); err != nil {
		// Stale cancel — idempotent success.
		s.ops.Observe(op, time.Since(start), nil)
		return &vmmdpb.CancelLiveMigrationResponse{}, nil
	}
	s.migrations.delete(req.GetInstanceId())
	s.ops.Observe(op, time.Since(start), nil)
	return &vmmdpb.CancelLiveMigrationResponse{}, nil
}

// leaseExpiryLoop sweeps the migration tracker on a 5-second
// tick and drops entries whose lease has expired. The
// canonical snapshot stays in storage until the per-vmmd
// snapshot-drift sweep reaps it.
//
// Started by the Server constructor next to the cpuCache /
// netCache / activity goroutines. Exits on vmmd shutdown.
func (s *Server) leaseExpiryLoop(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			entries := s.migrations.listExpired(now)
			for _, m := range entries {
				s.migrations.delete(m.instanceID)
				s.log.Info("vmmd: migration lease expired",
					"instance_id", m.instanceID,
					"lease_seconds", int(time.Since(m.createdAt).Seconds()),
				)
			}
		}
	}
}
