package meter

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// CPUSource is the per-instance cumulative CPU-µs reader the sampler
// uses to compute the per-minute delta. Production wires this to
// pkg/sched/instancestats.Reader via a thin adapter (pkg/meter/sampler.go
// takes a func, so the schedd package stays decoupled from pkg/meter).
// ok=false means the reader has no row for this instance (the
// instance is gone, or the schedd poller has not yet observed it).
type CPUSource interface {
	// CPUUsageUsec returns the cumulative host cgroup CPU-µs for
	// instanceID on the most recent schedd poll, plus a "found"
	// boolean. false means the reader has no row for this instance
	// this tick.
	CPUUsageUsec(instanceID string) (uint64, bool)
}

// EgressSource (ADR-046, step 8) is the per-instance egress-byte
// reader the sampler uses to compute the per-minute byte delta
// for usage_minutes.tx_bytes (gateway response bytes) and
// usage_minutes.net_tx_bytes (netns tap0 egress from vmmd). The
// two columns are sourced independently: production wires
// scheddEgressAdapter (reads netTxBytes from scheddgrpc.
// InstanceStatsRow, which vmmd populated from
// pkg/fcvm/netstats.Cache) and gatewayEgressAdapter (reads the
// gateway's per-instance ring buffer). ok=false means the
// reader has no row for this instance this tick (gone, never
// polled, regression). The TX and NetTX returned values are
// already per-tick deltas; the sampler appends them additively
// to the (instance, minute) row.
type EgressSource interface {
	// EgressBytes returns (txBytes, netTxBytes, ok). txBytes is
	// the gateway-side HTTP response byte count for this
	// instance this tick; netTxBytes is the root-side
	// vethHost.rx_bytes delta for this instance this tick. The
	// sampler does NOT compute a delta itself — the readers
	// own regression handling (mirroring cpustats's
	// drop-baseline contract). nil is OK for either field when
	// the corresponding source has no data; the sampler
	// writes 0 for that column and stamps the other normally.
	EgressBytes(instanceID string) (txBytes, netTxBytes uint64, ok bool)
}

// Sampler writes one minute of billable usage per live instance. It walks
// every app on the box (one-box scale; schedd's ListAllApps is the canonical
// source) and lists its instances; for each one in a state that counts
// against the RAM ledger it appends (ram_mb + 8) * 60 mb_seconds to
// usage_minutes for the truncated minute.
//
// Billing rule (spec §4.7): bill on plan RAM + 8 MB, not sampled RSS. The
// admission MB is the source of truth — schedd's ledger already charges the
// same number, so a row in usage_minutes matches what schedd counted toward
// invariant §6.2-2. Tests assert this parity.
//
// PR #75 (#71 in flight on this branch at PR open): the inline
// `ram_mb + api.PerVMOverheadMB` constant folded into api.BillableRAMMB; the
// AppendUsage idempotency on (instance_id, minute) is the meterd↔storage
// contract that prevents silent double-billing under any restart — see
// pkg/state/store.go::Store.AppendUsage.
//
// Issue #279 / PR-B: the sampler also reads the per-instance
// cumulative CPU-µs from a CPUSource (production: schedd
// instancestats.Reader) and appends the per-minute delta to
// usage_minutes.cpu_usec. cpu_usec is a measurement, NOT a billable
// unit — the financial model still bills on plan RAM + 8 MB
// (pkg/api/limits.go). The data lands in usage_minutes because that
// is the canonical per-(account, app, instance, minute) table; the
// read path is wired so a follow-up PR can extend
// Provider.PushUsageRecord without re-plumbing sampling.
type Sampler struct {
	store state.Store
	// cpu is the per-instance CPU-µs reader. nil is OK — the sampler
	// skips the CPU walk and writes 0; this is the test-harness
	// convention (no schedd in unit tests).
	cpu CPUSource
	// egress is the per-instance egress-byte reader
	// (ADR-046, step 8). nil is OK — the sampler writes 0
	// for both egress columns; the test-harness convention.
	egress EgressSource
	now    func() time.Time // injectable for tests

	// cpuBaselineMu guards the per-(instance, minute) baseline the
	// sampler uses to compute the per-minute CPU delta. The map is
	// keyed by instanceID; the value is the (lastSeenCPUUsec,
	// lastSeenMinute) pair stamped when the previous minute boundary
	// was crossed. mu is held only across the baseline lookup /
	// update — the AppendUsage call itself is unlocked (the store
	// owns its own concurrency).
	cpuBaselineMu sync.Mutex
	cpuBaseline   map[string]cpuBaseline
}

// cpuBaseline is the per-instance baseline the sampler retains
// across ticks. The cumulative counter
// (pkg/sched/instancestats.InstanceStat.CPUUsageUsec) is
// monotonically increasing across the lifetime of one cgroup; on a
// cgroup recreation (jailer restart, manual rmdir) it resets to a
// smaller number. The sampler treats the reset as a fresh baseline
// for the next minute — see SampleAndRoll for the regression branch.
type cpuBaseline struct {
	// lastCPUUsec is the cumulative CPU-µs the reader reported at
	// the previous tick. The per-minute delta is
	// `currCPUUsec - lastCPUUsec` (clamped to 0 on regression).
	lastCPUUsec uint64
	// lastMinute is the minute boundary the previous tick was
	// stamped with. The sampler resets the baseline ONLY when the
	// minute boundary changes — a redelivered minute (meterd
	// restart) sees the same baseline and idempotently writes
	// the same delta.
	lastMinute time.Time
}

// NewSampler wires the sampler. now defaults to time.Now if nil. cpu
// may be nil — the sampler skips the CPU walk and writes 0; this is
// the test-harness convention (no schedd in unit tests). egress may
// be nil — the sampler writes 0 for both egress columns; same
// convention. PR-2 (ADR-046) replaces this with a 4-arg signature
// `NewSampler(store, cpu, egress, now)` that wires both seams.
func NewSampler(store state.Store, cpu CPUSource, now func() time.Time) *Sampler {
	if now == nil {
		now = time.Now
	}
	return &Sampler{store: store, cpu: cpu, now: now}
}

// NewSamplerWithEgress is the ADR-046 wiring: store + cpu + egress +
// now. cmd/meterd passes scheddEgressAdapter and
// gatewayEgressAdapter here. now defaults to time.Now if nil. cpu
// and egress may both be nil — the sampler writes 0 for the
// respective column; the test-harness convention. The legacy
// NewSampler is kept for callers (cmd/meterd, e2e tests) that have
// not yet been migrated to the 4-arg form.
func NewSamplerWithEgress(store state.Store, cpu CPUSource, egress EgressSource, now func() time.Time) *Sampler {
	if now == nil {
		now = time.Now
	}
	return &Sampler{store: store, cpu: cpu, egress: egress, now: now}
}

// RolledRow is one (instance, minute) billable line. Returned alongside any
// error so callers (the test surface, telemetry) can observe what was
// billed without re-reading the store.
type RolledRow struct {
	InstanceID  string
	AppID       string
	AccountID   string
	Minute      time.Time
	MBSeconds   int64
	AdmissionMB int
	// CPUUsec is the per-minute CPU-µs delta the sampler appended
	// to usage_minutes.cpu_usec. Zero when the scheduler reader
	// has no row for this instance this tick (test stubs, or the
	// instance has not yet been polled). Issue #279 / PR-B.
	CPUUsec int64
	// TXBytes (ADR-046, step 8) is the per-minute
	// HTTP-response byte delta the sampler appended to
	// usage_minutes.tx_bytes. Source: gateway
	// statusRecorder.Bytes. Zero when no source is wired
	// or the source has no data this tick.
	TXBytes int64
	// NetTxBytes (ADR-046, step 8) is the per-minute
	// byte delta on root-side vethHost.rx_bytes the
	// sampler appended to usage_minutes.net_tx_bytes.
	// Source: vmmd netstats.Cache → schedd
	// instancestats.Poller → scheddgrpc.InstanceStatsRow.
	// Zero when no source is wired or the source has no
	// data this tick.
	NetTxBytes int64
}

// SampleAndRoll walks every app's live instances and appends one minute of
// billable usage per instance to usage_minutes. It is safe to call from a
// single goroutine; the Store implementation is responsible for concurrent
// safety (MemStore holds a single mutex; PgStore's INSERT … ON CONFLICT is
// atomic per row).
//
// The function returns the rows it wrote so tests can assert on the
// exact set without re-querying; production logs the count and moves on.
//
// Two side-effects per (instance, minute) row:
//   - the billable MB-seconds (spec §4.7: plan RAM + 8 MB per running
//     second; NOT changed by this PR).
//   - the per-minute CPU-µs delta (issue #279 / PR-B; informational
//     only — billing is on RAM).
func (s *Sampler) SampleAndRoll(ctx context.Context) ([]RolledRow, error) {
	minute := MinuteKey(s.now())
	apps, err := s.store.ListAllApps(ctx)
	if err != nil {
		return nil, err
	}
	var out []RolledRow
	for _, app := range apps {
		if app.Status == state.AppDeleted {
			continue
		}
		ins, err := s.store.ListInstancesForApp(ctx, app.ID)
		if err != nil {
			return nil, err
		}
		for _, ins := range ins {
			if !state.State(ins.State).CountsForRAM() {
				continue
			}
			row := RolledRow{
				InstanceID:  ins.ID,
				AppID:       app.ID,
				AccountID:   app.AccountID,
				Minute:      minute,
				AdmissionMB: api.BillableRAMMB(ins.RAMMB),
				MBSeconds:   MBSecondsPerMinute(api.BillableRAMMB(ins.RAMMB)),
			}
			// Move 1 (event-driven packaging): set usage_minutes.requests
			// to the count of invocations the drain drove through this
			// instance in this minute. Index-backed by
			// invocations_instance_idx (state='dispatching'). For
			// instances with zero traffic (just parked, not yet woken)
			// this returns 0 — matching the existing free-tier
			// semantics.
			requests, err := s.store.CountInstanceInvocationsInMinute(ctx, ins.ID, minute)
			if err != nil {
				return out, fmt.Errorf("meter: sample %s/%s: %w", app.ID, ins.ID, err)
			}
			row.CPUUsec = s.cpuDeltaForMinute(ins.ID, minute)
			// PR 2 (ADR-046): wire EgressSource so this row
			// carries tx_bytes (gateway response bytes) and
			// net_tx_bytes (root-side vethHost.rx_bytes
			// delta) instead of zeros. nil EgressSource
			// keeps the legacy PR-1 behaviour for tests +
			// cmd/meterd prior to the 4-arg wiring.
			// egressBytes returns (0, 0, false) when no source
			// is wired (the legacy PR-1 path) or the source has
			// no row for this instance; in both cases the
			// additive-merge baseline stays put (mirrors the
			// cpu path's contract — same idempotency).
			if txBytes, netTxBytes, ok := s.egressBytes(ins.ID); ok {
				row.TXBytes = int64(txBytes)
				row.NetTxBytes = int64(netTxBytes)
			}
			if err := s.store.AppendUsage(ctx, app.AccountID, app.ID, ins.ID, minute, row.MBSeconds, int64(requests), row.CPUUsec, row.TXBytes, row.NetTxBytes); err != nil {
				return out, err
			}
			out = append(out, row)
		}
	}
	return out, nil
}

// cpuDeltaForMinute computes the per-minute CPU-µs delta for the
// given instance and stamps the (instance, minute) baseline so the
// next call sees the diff from the previous tick. Returns 0 when
// the cpu source is nil (production: schedd reader not wired;
// tests), or when the reader has no row for this instance. The
// regression branch (currCPUUsec < lastCPUUsec) treats the new
// reading as a fresh baseline and returns 0 — the next minute
// picks up from there.
func (s *Sampler) cpuDeltaForMinute(instanceID string, minute time.Time) int64 {
	if s.cpu == nil {
		return 0
	}
	curr, ok := s.cpu.CPUUsageUsec(instanceID)
	if !ok {
		// Reader has no row for this instance (gone, or never
		// polled). Skip the baseline update so a future tick
		// that does observe it starts fresh.
		return 0
	}
	s.cpuBaselineMu.Lock()
	defer s.cpuBaselineMu.Unlock()
	if s.cpuBaseline == nil {
		s.cpuBaseline = map[string]cpuBaseline{}
	}
	prev, have := s.cpuBaseline[instanceID]
	var delta uint64
	switch {
	case !have:
		// First observation: this is the baseline. The first
		// non-zero delta is reported NEXT minute — same shape
		// as the vmmd cpustats.Cache first-sample-is-baseline
		// contract.
		delta = 0
	case curr < prev.lastCPUUsec:
		// Regression: cgroup recreated (jailer restart).
		// Mirrors pkg/fcvm/cpustats.Cache.Observe's
		// drop-baseline contract (ADR-039 §3.1) — the
		// customer's CPU clock for the instance starts fresh
		// on the new cgroup. The previous counter's work is
		// not patched across the break; the next-minute
		// delta picks up from the new counter.
		delta = 0
	case minute.Equal(prev.lastMinute):
		// Same minute boundary as the previous tick (redelivered
		// minute from a meterd restart). The delta is the full
		// curr - prev — restoring the previous baseline is
		// idempotent because AppendUsage on the same
		// (instance_id, minute) is additive on cpu_usec only
		// (DO NOTHING for mb_seconds / requests).
		delta = curr - prev.lastCPUUsec
	default:
		// New minute boundary crossed. The per-minute delta is
		// curr - prev; on a long gap (instance was parked
		// between minutes) the counter stops incrementing
		// (cgroup is gone) and curr equals prev → delta is 0,
		// which is the correct value for "no CPU consumed
		// during the gap".
		delta = curr - prev.lastCPUUsec
	}
	s.cpuBaseline[instanceID] = cpuBaseline{
		lastCPUUsec: curr,
		lastMinute:  minute,
	}
	return int64(delta)
}

// egressBytes returns (txBytes, netTxBytes, ok) for the given
// instance, mirroring the cpuDeltaForMinute shape but without
// the baseline state: the source readers (vmmd netstats.Cache
// via schedd, the gateway ring buffer) own their own regression
// handling, so the sampler is just a fan-out. Returns 0, 0, false
// when s.egress is nil (legacy PR-1 wiring; tests). Returns 0, 0,
// false when the source has no row for the instance (gone /
// never-polled). The sampler does NOT cache per-instance state;
// the source readers are themselves per-(instance, minute)
// accumulators on the producer side (mirror of the cpu baseline
// but moved across the wire boundary so the per-tick delta is
// computed where the counter lives).
func (s *Sampler) egressBytes(instanceID string) (uint64, uint64, bool) {
	if s.egress == nil {
		return 0, 0, false
	}
	return s.egress.EgressBytes(instanceID)
}
