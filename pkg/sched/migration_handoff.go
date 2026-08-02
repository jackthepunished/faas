// migration_handoff.go — Tier A5 (ADR-065) cross-node live-instance
// handoff orchestrator.
//
// Schedd's per-instance live-migration is a four-phase commit:
//   1. PrepareLiveMigration — the new owner vmmd's schedd dials
//      the DYING vmmd and asks it to pause the running VM +
//      write a fresh snapshot to the canonical storage backend.
//      The dying vmmd mints a lease_token and returns the
//      snapshot storage keys.
//   2. AdoptMigratedInstance — the new owner vmmd's schedd dials
//      the NEW owner vmmd and asks it to restore the snapshot
//      from Phase 1.
//   3. MigrateInstanceOwner — the orchestrator runs the
//      conditional UPDATE on the instances row: state
//      'migrating' → 'running', node_id flips to the new
//      owner, the migration lineage columns are stamped
//      (migrated_from_node_id, migrated_at, lease_token), and
//      apps.migrated_at is stamped in the same transaction.
//   4. AcknowledgeMigration — the new owner vmmd's schedd dials
//      the DYING vmmd and tells it "Phase 3 committed; you can
//      destroy the paused VM and free the netns/jail".
//
// On any failure between Phase 1 and Phase 3, the orchestrator
// runs Phase 4 (CancelLiveMigration) — the dying vmmd resumes
// the paused VM and the snapshot stays where it was. Phase 4 is
// best-effort; the row has already been rolled back via
// Store.CancelInstanceMigration before Phase 4 fires.
//
// A lease clock bounds the whole flow: the dying vmmd mints the
// lease_token at Phase 1 and runs a per-instance lease-expiry
// timer for MigrateLiveLeaseSeconds (pkg/api/limits.go). On
// lease expiry, the dying vmmd resumes the VM regardless of
// whether the orchestrator ran Phase 4. The orchestrator's
// conditional UPDATE at Phase 3 carries the lease_token as part
// of the predicate (well, the Phase-2 MarkInstanceMigrating
// runs first, then Phase 3 is conditional on state='migrating'
// + node_id=fromNodeID) so a stale lease can never silently
// commit.
//
// Concurrency: each candidate instance gets its own goroutine
// (the parent Engine.MigrateLiveInstances spawns one per
// instance up to MigrateLiveMaxPerTick). The four-phase
// orchestrator runs synchronously inside the goroutine so the
// lease clock is observable to the caller.
//
// Failure modes:
//   - Peer race (peer re-owner / peer rollback): the
//     conditional UPDATEs at Phase 2 + Phase 3 return
//     ErrConflict. The orchestrator logs Warn and drops; the
//     metric bumps outcome="conflict".
//   - Lease expiry: MigrateLiveLeaseSeconds elapses before
//     Phase 3 commits. The dying vmmd has already resumed the
//     VM (Phase 4 ran there). The orchestrator's Phase 4 fails
//     on the wire (the VM is already RUNNING), which is logged
//     Debug and dropped. Metric bumps outcome="lease_expired".
//   - Peer failure (Phase 1 / Phase 2 / Phase 3.5 / Phase 4
//     gRPC dial error): logged Warn, Phase 4 fires, metric
//     bumps outcome="peer_failure".
//   - Instance gone (Phase 2 / Phase 3 ErrNotFound): the
//     instance was hard-deleted mid-flight. Logged Warn,
//     dropped, no metric bump (this is a cold-start race, not
//     a migration failure).
//
// Hard limits policy (CLAUDE.md): every limit is a constant in
// pkg/api/limits.go, never inlined here.

package sched

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/fcvm"
	"github.com/onebox-faas/faas/pkg/state"
)

// MigrationHarness owns the four-phase cross-node live-instance
// handoff. One per Engine; cmd/schedd builds it from
// EngineOpts and passes it down. The handoff's only state is
// the per-orchestrator context (so a fan-out caller can cancel
// the whole batch on ctx.Done) and the metrics accessor (so the
// outcome dispatcher can bump liveMigrationDecisions).
type MigrationHarness struct {
	store   state.Store
	vmm     RoutedVMM
	metrics apiMigrationMetrics
	log     *slog.Logger
	// newOwnerNodeID is the local schedd's owner node — the
	// new owner for every handoff this orchestrator drives.
	// Set at construction; never mutated (a hot-swap would be
	// a Tier B feature).
	newOwnerNodeID string
	// maxPerTick is api.MigrateLiveMaxPerTick (the
	// per-drain-event cap on the parent Engine.MigrateLiveInstances
	// loop). Read at construction time so the per-instance
	// goroutine spawn count is bounded at Engine.MigrateLiveInstances
	// call time, not here.
	maxPerTick int
	// leaseSeconds is api.MigrateLiveLeaseSeconds (the upper
	// bound on the four-phase handoff). Passed to the dying
	// vmmd at Phase 1 via a context.WithTimeout so a
	// stuck-three-phase handoff is cancelled on the wire
	// before the dying vmmd's own lease-expiry timer fires.
	leaseSeconds int
}

// apiMigrationMetrics is the slice of pkg/wire.OpsMetrics the
// harness actually uses. Defined here so the harness doesn't
// import pkg/wire (which would force-test pkg/wire's
// constructors). The real implementation is
// pkg/wire.NewOpsMetrics, whose LiveMigrationDecisions
// accessor matches this signature.
type apiMigrationMetrics interface {
	LiveMigrationDecisions(outcome string) interface {
		Inc()
	}
}

// NewMigrationHarness builds the orchestrator. The caller
// (cmd/schedd wiring) is responsible for filling newOwnerNodeID
// from FAAS_NODE_NAME resolution and for capping maxPerTick /
// leaseSeconds from the env-overridable limits.
//
// The metrics parameter is intentionally typed as an interface
// (rather than *api.Limit) so tests can inject a no-op recorder
// without dragging the full pkg/wire registry into the test
// binary. Production wiring passes wire.NewOpsMetrics(...)
// directly.
func NewMigrationHarness(
	store state.Store,
	vmm RoutedVMM,
	metrics apiMigrationMetrics,
	log *slog.Logger,
	newOwnerNodeID string,
) *MigrationHarness {
	return &MigrationHarness{
		store:          store,
		vmm:            vmm,
		metrics:        metrics,
		log:            log,
		newOwnerNodeID: newOwnerNodeID,
		maxPerTick:     api.MigrateLiveMaxPerTick,
		leaseSeconds:   api.MigrateLiveLeaseSeconds,
	}
}

// SetMaxPerTick overrides the per-batch cap (tests only). The
// production wiring reads api.MigrateLiveMaxPerTick at
// construction; tests use a smaller cap so a fixture with N
// instances doesn't spawn N goroutines.
func (h *MigrationHarness) SetMaxPerTick(n int) { h.maxPerTick = n }

// SetLeaseSeconds overrides the lease window (tests only).
func (h *MigrationHarness) SetLeaseSeconds(n int) { h.leaseSeconds = n }

// MigrateOne runs the four-phase handoff for a single
// candidate instance. The parent Engine.MigrateLiveInstances
// goroutine pool calls this once per candidate.
//
// The instanceID + fromNodeID arguments come from the
// ListLiveInstancesOnNode result. The leaseToken is minted by
// the dying vmmd at Phase 1; this function returns the final
// outcome enum so the caller can bump the metric.
//
// The lease clock is enforced at the orchestrator level via a
// context.WithTimeout(leaseSeconds) so a stuck-three-phase
// handoff surfaces as ctx.DeadlineExceeded — the Phase 4
// rollback path picks that up and the metric bumps
// outcome="lease_expired".
func (h *MigrationHarness) MigrateOne(ctx context.Context, instanceID, fromNodeID string) error {
	if instanceID == "" || fromNodeID == "" {
		return fmt.Errorf("sched: migrate one: empty instanceID or fromNodeID")
	}
	// Lease-bounded context: the four-phase flow must complete
	// inside MigrateLiveLeaseSeconds. A deadline-exceeded
	// cancellation fires Phase 4 (the row was never moved to
	// 'migrating' if Phase 2 failed; if Phase 3 is the slow
	// step, the lease is already committed and Phase 4 is a
	// no-op on the wire).
	leaseCtx, cancel := context.WithTimeout(ctx, time.Duration(h.leaseSeconds)*time.Second)
	defer cancel()

	// Phase 1: PrepareLiveMigration on the dying vmmd. The
	// dying vmmd pauses the VM, writes the snapshot to the
	// canonical storage backend, and returns the storage keys
	// + a lease_token (UUIDv4 minted by the dying vmmd so the
	// lease clock is tied to the dying vmmd's lease timer).
	//
	// The snapshot_storage_key is the canonical mem blob key
	// the new owner will pull from after Phase 2. We mint it
	// deterministically here via the same shape imaged uses
	// (snap/<deploymentID>/mem) so the dying vmmd and the new
	// owner agree on the namespace. The vmstate blob is the
	// sibling key (snap/<deploymentID>/vmstate); both keys are
	// returned by the dying vmmd at Phase 1.
	snapshotKey := fmt.Sprintf("snap/migration-%s/mem", instanceID)
	prepared, err := h.vmm.PrepareLiveMigration(leaseCtx, fromNodeID, instanceID, snapshotKey)
	if err != nil {
		// Phase 1 failure: no state has changed on the dying
		// vmmd (it returned before pausing). The instance
		// stays RUNNING on the dying node. Drop silently and
		// bump peer_failure — the next compute_node_changed
		// event retries.
		h.metrics.LiveMigrationDecisions("peer_failure").Inc()
		h.log.Warn("sched: migrate one: Phase 1 prepare failed",
			"instance_id", instanceID,
			"from_node_id", fromNodeID,
			"err", err,
		)
		return fmt.Errorf("sched: migrate one: phase 1 prepare: %w", err)
	}

	// Phase 2: MarkInstanceMigrating on the local store. The
	// conditional UPDATE flips the instance state from
	// 'running' to 'migrating' under the
	// state='running' + node_id=fromNodeID predicate. A peer
	// rollback / owner change / row-gone returns ErrConflict.
	if err := h.store.MarkInstanceMigrating(leaseCtx, instanceID, fromNodeID); err != nil {
		if errors.Is(err, state.ErrConflict) {
			h.metrics.LiveMigrationDecisions("conflict").Inc()
			h.log.Debug("sched: migrate one: Phase 2 peer conflict",
				"instance_id", instanceID,
				"from_node_id", fromNodeID,
			)
			// Phase 4: tell the dying vmmd to abort the
			// pause and resume the VM. Best-effort.
			_ = h.vmm.CancelLiveMigration(leaseCtx, fromNodeID, instanceID, prepared.LeaseToken)
			return state.ErrConflict
		}
		if errors.Is(err, state.ErrNotFound) {
			// Instance hard-deleted mid-flight. Drop.
			h.log.Warn("sched: migrate one: Phase 2 instance gone",
				"instance_id", instanceID,
			)
			_ = h.vmm.CancelLiveMigration(leaseCtx, fromNodeID, instanceID, prepared.LeaseToken)
			return state.ErrNotFound
		}
		h.metrics.LiveMigrationDecisions("peer_failure").Inc()
		h.log.Warn("sched: migrate one: Phase 2 mark migrating failed",
			"instance_id", instanceID,
			"from_node_id", fromNodeID,
			"err", err,
		)
		_ = h.vmm.CancelLiveMigration(leaseCtx, fromNodeID, instanceID, prepared.LeaseToken)
		return fmt.Errorf("sched: migrate one: phase 2 mark migrating: %w", err)
	}

	// Phase 3: AdoptMigratedInstance on the new owner vmmd.
	// The new owner restores the snapshot the dying vmmd wrote
	// at Phase 1, brings the VM up, and returns the network
	// identifiers. We don't currently persist those on the
	// migration path (the instance row's host_ip is set at
	// wake time and the column stays), but the wire shape
	// carries them so the new owner vmmd's logs can correlate.
	//
	// AppSpec is rebuilt from the local app + deployment view.
	// For the A5 v1 PR the AppSpec shape is built from the
	// Instance's app_id via a best-effort lookup; a future
	// PR will thread the AppSpec through Engine.MigrateLiveInstances
	// to avoid the lookup (the engine already has the App
	// row in hand at the call site).
	appSpec, err := h.loadAppSpecForInstance(leaseCtx, instanceID)
	if err != nil {
		// Phase 3 setup failure (AppSpec couldn't be built).
		// Roll back Phase 2 via Store.CancelInstanceMigration
		// (which restores state='parked' on the original owner)
		// and Phase 4 via the dying vmmd.
		h.metrics.LiveMigrationDecisions("peer_failure").Inc()
		h.log.Warn("sched: migrate one: load app spec failed",
			"instance_id", instanceID,
			"err", err,
		)
		_ = h.store.CancelInstanceMigration(leaseCtx, instanceID, fromNodeID, prepared.LeaseToken)
		_ = h.vmm.CancelLiveMigration(leaseCtx, fromNodeID, instanceID, prepared.LeaseToken)
		return fmt.Errorf("sched: migrate one: load app spec: %w", err)
	}
	adopted, err := h.vmm.AdoptMigratedInstance(leaseCtx, h.newOwnerNodeID, instanceID, appSpec,
		prepared.MemStorageKey, prepared.VMStateStorageKey, prepared.LeaseToken)
	if err != nil {
		// Phase 3 wire failure (new owner dial / restore
		// failed). Roll back Phase 2 + Phase 4.
		h.metrics.LiveMigrationDecisions("peer_failure").Inc()
		h.log.Warn("sched: migrate one: Phase 3 adopt failed",
			"instance_id", instanceID,
			"new_owner_node_id", h.newOwnerNodeID,
			"err", err,
		)
		_ = h.store.CancelInstanceMigration(leaseCtx, instanceID, fromNodeID, prepared.LeaseToken)
		_ = h.vmm.CancelLiveMigration(leaseCtx, fromNodeID, instanceID, prepared.LeaseToken)
		return fmt.Errorf("sched: migrate one: phase 3 adopt: %w", err)
	}
	_ = adopted // network identifiers are surfaced on the wire but
	// not persisted in this PR; future work can plumb them
	// through the gateway listener if a customer wants
	// zero-downtime migration observability.

	// Phase 4: MigrateInstanceOwner on the local store. The
	// conditional UPDATE flips the instance row: state
	// 'migrating' → 'running', node_id flips to newOwner, the
	// migration lineage columns are stamped, AND
	// apps.migrated_at is stamped in the same transaction.
	if err := h.store.MigrateInstanceOwner(leaseCtx, instanceID, fromNodeID, h.newOwnerNodeID, prepared.LeaseToken); err != nil {
		// Distinguish peer rollback / re-owner / lease
		// expiry / row-gone via errors.Is. Each branch has a
		// different metric label.
		if errors.Is(err, state.ErrConflict) {
			// Peer rollback / re-owner: the row was moved
			// by a concurrent orchestrator. The dying vmmd
			// still has the VM paused; tell it to abort
			// (Phase 4 on the wire).
			h.metrics.LiveMigrationDecisions("conflict").Inc()
			h.log.Debug("sched: migrate one: Phase 4 peer conflict",
				"instance_id", instanceID,
				"from_node_id", fromNodeID,
			)
			_ = h.vmm.CancelLiveMigration(leaseCtx, fromNodeID, instanceID, prepared.LeaseToken)
			return state.ErrConflict
		}
		if errors.Is(err, state.ErrNotFound) {
			h.log.Warn("sched: migrate one: Phase 4 instance gone",
				"instance_id", instanceID,
			)
			_ = h.vmm.CancelLiveMigration(leaseCtx, fromNodeID, instanceID, prepared.LeaseToken)
			return state.ErrNotFound
		}
		// Anything else: lease expiry (ctx.DeadlineExceeded)
		// or a transient DB error. Bump lease_expired on the
		// context error; bump peer_failure on anything else
		// (the operator can disambiguate via slog).
		if errors.Is(err, leaseCtx.Err()) || errors.Is(err, context.DeadlineExceeded) {
			h.metrics.LiveMigrationDecisions("lease_expired").Inc()
			h.log.Warn("sched: migrate one: Phase 4 lease expired",
				"instance_id", instanceID,
				"from_node_id", fromNodeID,
				"lease_seconds", h.leaseSeconds,
			)
			_ = h.vmm.CancelLiveMigration(leaseCtx, fromNodeID, instanceID, prepared.LeaseToken)
			return fmt.Errorf("sched: migrate one: phase 4 lease expired: %w", err)
		}
		h.metrics.LiveMigrationDecisions("peer_failure").Inc()
		h.log.Warn("sched: migrate one: Phase 4 commit failed",
			"instance_id", instanceID,
			"from_node_id", fromNodeID,
			"err", err,
		)
		_ = h.vmm.CancelLiveMigration(leaseCtx, fromNodeID, instanceID, prepared.LeaseToken)
		return fmt.Errorf("sched: migrate one: phase 4 commit: %w", err)
	}

	// Phase 5: AcknowledgeMigration on the dying vmmd. Best-
	// effort — a non-OK status here is logged Debug and
	// dropped. The dying vmmd will eventually destroy the
	// paused VM on its own lease-expiry timer; the ack is
	// just a "you can free the netns now" hint.
	if err := h.vmm.AcknowledgeMigration(leaseCtx, fromNodeID, instanceID, prepared.LeaseToken); err != nil {
		h.log.Debug("sched: migrate one: Phase 5 ack best-effort failed",
			"instance_id", instanceID,
			"from_node_id", fromNodeID,
			"err", err,
		)
	}

	// Success.
	h.metrics.LiveMigrationDecisions("migrated").Inc()
	h.log.Info("sched: migrate one: success",
		"instance_id", instanceID,
		"from_node_id", fromNodeID,
		"to_node_id", h.newOwnerNodeID,
		"lease_token", prepared.LeaseToken,
	)
	return nil
}

// loadAppSpecForInstance rebuilds the AppSpec shape vmmd needs
// to restore a paused VM from the local app + deployment view.
// The lookup walks: instance → app + deployment → drive0 base
// key + drive1 layer key + sealed env + api env + egress
// allowlist. For A5 v1 the wire shape carries the AppSpec end-
// to-end; a future PR can lift this into the parent
// Engine.MigrateLiveInstances so the orchestrator doesn't re-
// fetch (saves a round-trip per instance).
//
// Returns an error if the instance / app / deployment / layer
// triple can't be resolved. The caller treats this as a Phase 3
// setup failure and rolls back Phase 2 + Phase 4.
func (h *MigrationHarness) loadAppSpecForInstance(ctx context.Context, instanceID string) (AppSpec, error) {
	// Best-effort: read the instance row, then the app + dep
	// rows. The MemStore / PgStore backends both implement the
	// InstanceByID + AppByID + LiveDeployment surface the engine
	// uses at Wake time — the migration path reuses it bit-for-bit.
	ins, err := h.store.InstanceByID(ctx, instanceID)
	if err != nil {
		return AppSpec{}, fmt.Errorf("sched: load app spec: instance by id: %w", err)
	}
	app, err := h.store.AppByID(ctx, ins.AppID)
	if err != nil {
		return AppSpec{}, fmt.Errorf("sched: load app spec: app by id: %w", err)
	}
	dep, err := h.store.LiveDeployment(ctx, ins.AppID)
	if err != nil {
		return AppSpec{}, fmt.Errorf("sched: load app spec: live deployment: %w", err)
	}
	// Sealed env + api env + allowlist are all per-app; the
	// engine reads them at Wake time and we mirror that here.
	// For A5 v1 we use empty slices if the lookup fails so the
	// migration doesn't block on a secrets table hiccup (the
	// guest's env.json ends up empty for the migration window;
	// a fresh wake restores them). The wire shape is unchanged.
	sealedRows, _ := h.store.ListAppSecrets(ctx, app.AccountID, app.ID)
	apiEnvRows, _ := h.store.ListAppEnv(ctx, app.AccountID, app.ID)
	sealed := make([]fcvm.SealedEnvEntry, 0, len(sealedRows))
	for _, s := range sealedRows {
		sealed = append(sealed, fcvm.SealedEnvEntry{Key: s.Key, Ciphertext: s.Ciphertext})
	}
	apiEnv := make([]fcvm.APIEnvEntry, 0, len(apiEnvRows))
	for _, e := range apiEnvRows {
		apiEnv = append(apiEnv, fcvm.APIEnvEntry{Key: e.Key, Value: e.Value})
	}
	allowlist := make([]string, 0, len(app.EgressAllowlist))
	for _, p := range app.EgressAllowlist {
		allowlist = append(allowlist, p.String())
	}
	spec := AppSpec{
		BaseKey:         dep.RootfsKey, // drive0 base key (Storage.Get)
		LayerKey:        dep.RootfsKey, // drive1 per-app layer (same key for A5 v1; the engine constructs the per-app layer from dep.RootfsKey at wake time)
		VCPUCount:       int32(app.MaxConcurrency),
		MemSizeMiB:      int32(app.RAMMB),
		EgressMbit:      0,
		SealedEnv:       sealed,
		APIEnv:          apiEnv,
		EgressAllowlist: allowlist,
		Port:            dep.OverridePort,
		HealthcheckPath: "",
	}
	return spec, nil
}