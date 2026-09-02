package gateway

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/sched"
)

// burstCapacityAdmitter is an optional gateway capability. The request path
// uses it only when the backend can admit more than one instance; legacy test
// backends and pre-burst adapters keep the existing one-instance behaviour.
// The backend remains the authority on capacity and may return fewer admits
// than requested when the scheduler or node ledger reaches a limit.
type burstCapacityAdmitter interface {
	AdmitBurst(ctx context.Context, appID, scope, trigger string, maxConcurrency, count int) (admitted int, err error)
}

// burstPressure tracks requests which have passed the edge rate limits and
// may still need a function target. It is deliberately local to gatewayd:
// unlike Prometheus, it is available immediately during a burst and does not
// depend on a scrape or a healthy control-plane metrics path.
type burstPressure struct {
	apps sync.Map // app id -> *burstPressureState
}

type burstPressureState struct {
	inflight atomic.Int64
	worker   atomic.Bool
}

func (p *burstPressure) state(appID string) *burstPressureState {
	if p == nil || appID == "" {
		return nil
	}
	value, _ := p.apps.LoadOrStore(appID, &burstPressureState{})
	return value.(*burstPressureState)
}

// begin records one request and returns its balanced release function. The
// state is intentionally retained after the count reaches zero: deployed-app
// cardinality is bounded, while deleting map entries on the hot path would
// introduce a load/store race with a concurrent burst worker.
func (p *burstPressure) begin(appID string) func() {
	state := p.state(appID)
	if state == nil {
		return func() {}
	}
	state.inflight.Add(1)
	return func() {
		state.inflight.Add(-1)
	}
}

func desiredBurstInstances(inflight int64, perVM, maxInstances int) int {
	if inflight <= 0 || perVM <= 0 || maxInstances <= 0 {
		return 0
	}
	desired := (inflight + int64(perVM) - 1) / int64(perVM)
	if desired > int64(maxInstances) {
		return maxInstances
	}
	return int(desired)
}

// maybeBurstCapacity starts one detached worker per app. The worker keeps
// reconciling desired capacity while the burst is present, admitting at most
// ScaleUpMaxBurstPerTick instances per scheduler round. This gives the public
// request path an immediate signal without creating one scheduler request per
// incoming HTTP request.
func (h *Handler) maybeBurstCapacity(ctx context.Context, app App, maxInstances, perVM int) {
	if h == nil || h.backend == nil || h.burstPressure == nil || app.ID == "" || maxInstances <= 0 || perVM <= 0 {
		return
	}
	admitter, ok := h.backend.(burstCapacityAdmitter)
	if !ok {
		return
	}
	state := h.burstPressure.state(app.ID)
	if state == nil {
		return
	}
	inflight := state.inflight.Load()
	healthy := h.backend.HealthyCount(app.ID)
	desired := desiredBurstInstances(inflight, perVM, maxInstances)
	if desired <= healthy || !state.worker.CompareAndSwap(false, true) {
		return
	}

	go func() {
		defer state.worker.Store(false)
		lifecycleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), admissionLifecycleTimeout)
		defer cancel()

		for lifecycleCtx.Err() == nil {
			inflight := state.inflight.Load()
			healthy := h.backend.HealthyCount(app.ID)
			desired := desiredBurstInstances(inflight, perVM, maxInstances)
			if desired <= healthy {
				return
			}
			count := desired - healthy
			if count > api.ScaleUpMaxBurstPerTick {
				count = api.ScaleUpMaxBurstPerTick
			}
			admitted, err := admitter.AdmitBurst(lifecycleCtx, app.ID, app.Scope, sched.TriggerGateway, maxInstances, count)
			if err != nil && h.log != nil {
				h.log.Warn("gateway: burst admission failed", "app_id", app.ID, "requested", count, "admitted", admitted, "err", err)
			}
			if admitted == 0 {
				return
			}
		}
	}()
}

// AdmitBurst runs a bounded set of scheduler admissions concurrently. The
// production schedd client preserves the scheduler's first-admit plus
// continuation semantics over the existing RPC; older Scheduler adapters
// fall back to concurrent single admissions. The schedd ledger remains the
// authoritative source for per-app and per-node limits.
func (b *PGBackend) AdmitBurst(ctx context.Context, appID, scope, trigger string, maxConcurrency, count int) (int, error) {
	if b == nil || appID == "" || maxConcurrency <= 0 || count <= 0 {
		return 0, nil
	}
	if count > api.ScaleUpMaxBurstPerTick {
		count = api.ScaleUpMaxBurstPerTick
	}
	// The production schedd client carries the scheduler's burst
	// continuation marker over gRPC. That preserves the existing
	// Engine.AdmitInstances contract: the first admission passes the
	// ordinary gates, while its siblings do not get rejected by the
	// same app's scale-out cooldown.
	if sched, err := b.resolveSched(ctx, appID); err != nil {
		return 0, err
	} else if burst, ok := sched.(burstScheduler); ok {
		var (
			mu       sync.Mutex
			admitted int
			firstErr error
		)
		err := burst.AdmitInstances(ctx, appID, scope, trigger, count,
			func(instanceID, nodeID, deploymentID, wakeID string, method int32, atCapacity bool, port int, admitErr error) {
				if admitErr != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = admitErr
					}
					mu.Unlock()
					return
				}
				_, _, atCap, recordErr := b.recordAdmission(ctx, appID, deploymentID, instanceID, nodeID, deploymentID, wakeID, method, atCapacity, port)
				mu.Lock()
				defer mu.Unlock()
				if recordErr != nil {
					if firstErr == nil {
						firstErr = recordErr
					}
					return
				}
				if !atCap {
					admitted++
				}
			})
		mu.Lock()
		defer mu.Unlock()
		if firstErr != nil {
			return admitted, firstErr
		}
		return admitted, err
	}

	type result struct {
		admitted bool
		err      error
	}
	results := make(chan result, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wakeID, _, atCapacity, err := b.Admit(ctx, appID, "", scope, trigger, maxConcurrency)
			results <- result{admitted: err == nil && !atCapacity && wakeID != "", err: err}
		}()
	}
	wg.Wait()
	close(results)

	admitted := 0
	var firstErr error
	for result := range results {
		if result.admitted {
			admitted++
		}
		if result.err != nil && firstErr == nil {
			firstErr = result.err
		}
	}
	return admitted, firstErr
}

var _ burstCapacityAdmitter = (*PGBackend)(nil)
