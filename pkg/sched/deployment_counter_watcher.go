// Package sched — deployment_counter_watcher.go is the event-driven
// reset trigger for the per-deployment 100% sampling window
// (issue #555 acceptance #5).
//
// Background: a new deployment's first 100 root spans are
// 100% sampled. The window must be reset on the transition
// "this deployment's last live instance has parked" so a
// re-deploy (new image, same app slug) starts fresh.
//
// The reset signal arrives via the in-process Platform `wake`
// topic (events.TopicWake). Schedd's engine already emits
// ParkCompleted on every successful park; the watcher consumes
// those events, decrements a per-deployment live-count cache, and
// when the post-park count reaches 0 calls counter.Reset on the
// shared DeploymentCounter that the otelinit sampler consults.
//
// Failure semantics: a missed event (subscriber buffer full,
// restart, pg_notify drift) is recoverable — the periodic resync
// against state.Store.CountLiveInstancesByDeployment re-anchors
// the cache every 5 minutes. The window is best-effort, not
// load-bearing: a missed reset just means the next deployment
// reuses the parent's window until the next park transition.
package sched

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/onebox-faas/faas/pkg/events"
	"github.com/onebox-faas/faas/pkg/wire/otelinit"
)

// wakeEventEnvelope mirrors the structure pkg/events/platform.go
// marshals onto the Broadcaster. Field names match the JSON keys
// (snake_case) — the watcher decodes a copy of the broadcast payload
// to extract the deployment_id without re-walking the full event
// schema.
//
// Topic is the canonical Platform kind (e.g. "wake.park_completed").
// The watcher filters on Kind == events.WakeParkCompleted.
type wakeEventEnvelope struct {
	At    time.Time `json:"at"`
	Kind  string    `json:"kind"`
	Actor string    `json:"actor"`
	Data  struct {
		AppID        string `json:"app_id"`
		DeploymentID string `json:"deployment_id"`
		InstanceID   string `json:"instance_id"`
	} `json:"data"`
}

// DeploymentCounterWatcher subscribes to the schedd Platform's
// in-process wake topic and resets the otelinit per-deployment
// sampling counter on the "last live instance parked" transition.
//
// The watcher maintains a per-deployment live-count cache, updated
// on every observed ParkCompleted. A periodic re-sync (every 5
// minutes) re-anchors the cache against state.Store to recover from
// missed events after a restart or a subscriber buffer drop.
//
// One DeploymentCounterWatcher per schedd daemon. Construct in
// cmd/schedd/main.go after engine.WithEvents so the Platform is
// publishing to the Broadcaster the watcher subscribes to.
type DeploymentCounterWatcher struct {
	broadcaster *events.Broadcaster
	counter     *otelinit.DeploymentCounter
	store       LiveInstanceCounter
	log         *slog.Logger
	resyncEvery time.Duration

	// liveCount is the per-(deployment_id) live-instance count
	// cache. Mutated under mu; reads from Run() are also under mu
	// to keep the surface small.
	mu        sync.Mutex
	liveCount map[string]int
}

// LiveInstanceCounter is the narrow surface the watcher needs
// from the state store. A production caller passes a function
// value built over state.Store.CountLiveInstancesByDeployment;
// tests pass a stub. Decoupling the watcher from the full
// state.Store interface keeps the test surface small (issue #555
// PR-6).
type LiveInstanceCounter interface {
	CountLiveInstancesByDeployment(ctx context.Context, deploymentID string) (int, error)
}

// NewDeploymentCounterWatcher wires the watcher. broadcaster may
// be nil — Run() then becomes a no-op (the watcher exists for
// test seam / lifecycle uniformity but does nothing). counter
// and store are required: a nil counter is a programming bug and
// panics via the underlying DeploymentCounter.Observe call.
func NewDeploymentCounterWatcher(
	broadcaster *events.Broadcaster,
	counter *otelinit.DeploymentCounter,
	store LiveInstanceCounter,
	log *slog.Logger,
) *DeploymentCounterWatcher {
	return &DeploymentCounterWatcher{
		broadcaster: broadcaster,
		counter:     counter,
		store:       store,
		log:         log,
		resyncEvery: 5 * time.Minute,
		liveCount:   make(map[string]int),
	}
}

// Run blocks until ctx is cancelled. It performs an initial
// resync (no resets — just populates the cache) and then enters
// the per-event loop + the periodic resync ticker.
//
// A nil broadcaster makes Run a no-op (returns nil when ctx is
// cancelled). This is the "Platform uses pg_notify only" path
// (the cmd/schedd/main.go default before PR-6): the watcher is
// constructed but does nothing because no in-process events
// arrive. The window still resets when schedd restarts (the
// per-deployment counter starts at zero after a cold start).
func (w *DeploymentCounterWatcher) Run(ctx context.Context) error {
	if w.broadcaster == nil {
		w.log.Info("deployment_counter_watcher: broadcaster nil; watcher is no-op")
		<-ctx.Done()
		return nil
	}
	if err := w.resync(ctx); err != nil {
		// Resync failure is non-fatal — the cache starts empty and
		// the per-event path will populate it. Log and continue.
		w.log.Warn("deployment_counter_watcher: initial resync failed",
			"err", err)
	}

	ch, cancel := w.broadcaster.Subscribe(events.TopicWake)
	defer cancel()

	ticker := time.NewTicker(w.resyncEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := w.resync(ctx); err != nil {
				w.log.Warn("deployment_counter_watcher: resync failed", "err", err)
			}
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			w.handle(ev.Payload)
		}
	}
}

// handle decodes a single event payload and applies the
// per-deployment live-count update + counter reset.
//
// ParkCompleted: decrement count; if post-decrement count == 0,
// call counter.Reset(deployment_id).
//
// All other kinds: ignored. The watcher exists for one transition.
func (w *DeploymentCounterWatcher) handle(payload []byte) {
	var env wakeEventEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		// Bad JSON is a programming bug on the emit side; log
		// and skip. The wake lifecycle is the source of truth;
		// the watcher is observation.
		w.log.Warn("deployment_counter_watcher: decode envelope", "err", err)
		return
	}
	if env.Kind != events.WakeParkCompleted {
		return
	}
	depID := env.Data.DeploymentID
	if depID == "" {
		// ParkCompleted without a deployment_id is a legacy
		// emit (the field is added in PR-6) — skip without
		// touching the cache.
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	w.liveCount[depID]--
	if w.liveCount[depID] < 0 {
		// Defensive: the cache should never underflow. A
		// missed ParkCompleted could leave the cache at a
		// stale positive count; the next resync will re-anchor.
		// Reset to 0 here to avoid permanent under-counts.
		w.liveCount[depID] = 0
	}
	if w.liveCount[depID] == 0 {
		delete(w.liveCount, depID)
		w.counter.Reset(depID)
		w.log.Info("deployment_counter_watcher: reset counter",
			"deployment_id", depID)
	}
}

// resync re-anchors the live-count cache against state.Store for
// every known deployment_id. The SQL query is bounded by the
// cache's key set (human-paced deployment turnover), so the cost
// is one count(*) per active deployment.
//
// New deployment_ids that the cache does not yet know are NOT
// added — the per-event path populates them. This keeps resync a
// pure "fix any drift" operation; it does not discover new
// deployments.
func (w *DeploymentCounterWatcher) resync(ctx context.Context) error {
	w.mu.Lock()
	deps := make([]string, 0, len(w.liveCount))
	for depID := range w.liveCount {
		deps = append(deps, depID)
	}
	w.mu.Unlock()

	if len(deps) == 0 {
		return nil
	}
	for _, depID := range deps {
		n, err := w.store.CountLiveInstancesByDeployment(ctx, depID)
		if err != nil {
			return err
		}
		w.mu.Lock()
		if n == 0 {
			delete(w.liveCount, depID)
			w.counter.Reset(depID)
			w.log.Info("deployment_counter_watcher: resync reset counter",
				"deployment_id", depID)
		} else {
			w.liveCount[depID] = n
		}
		w.mu.Unlock()
	}
	return nil
}
