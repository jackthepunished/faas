package instancestats

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"math"
	"time"

	"github.com/onebox-faas/faas/pkg/sched"
	"github.com/onebox-faas/faas/pkg/state"
	"github.com/onebox-faas/faas/pkg/wire"
)

// DefaultStatsInterval is the per-Tick cadence (issue #170 / PR-A).
// 200 ms = 5 Hz; the 250 ms spike-capture acceptance gate in issue
// #170's metal test passes at one or two ticks. A future #172 config
// knob (StatsInterval) will plumb this through cmd/schedd without
// touching the Reader API.
const DefaultStatsInterval = 200 * time.Millisecond

// DefaultFreshness is the staleness budget a future PruneOlderThan
// gate would use. Today the poller never stamps Stale; the budget
// is plumbed through so adding the gate later does not require a
// signature change.
const DefaultFreshness = 5 * time.Second

// Dialer is the per-tick per-node VMM transport factory. Mirrors
// pkg/sched.HeartbeatDialer (issue #120 / PR #122): the poller
// dials fresh per Tick and closes the VMM when the sweep is done,
// so vmmd conn churn is bounded by the dialer. cmd/schedd passes
// HeartbeatDialerFunc(deps.dialVMM) so the production closure
// (overlay.Dial) is reused bit-for-bit — no second dial primitive
// per call site.
type Dialer interface {
	Dial(ctx context.Context, targetURL string, tlsCfg *tls.Config) (sched.VMM, error)
}

// DialerFunc adapts an ordinary function to the Dialer interface.
// Same precedent as HeartbeatDialerFunc in pkg/sched/heartbeat.go.
type DialerFunc func(ctx context.Context, targetURL string, tlsCfg *tls.Config) (sched.VMM, error)

// Dial implements Dialer.
func (f DialerFunc) Dial(ctx context.Context, targetURL string, tlsCfg *tls.Config) (sched.VMM, error) {
	return f(ctx, targetURL, tlsCfg)
}

// Poller is the periodic instance-stats worker. Mirrors
// pkg/sched.Heartbeat in shape: Tick does one full sweep; Run
// loops Tick on a fixed interval until ctx is done. The poller
// holds per-instance state for CPU delta math; the state is
// process-local and resets on process restart (no durable
// baseline — the cgroup cumulative counter survives).
type Poller struct {
	Interval  time.Duration
	Store     state.Store
	Dialer    Dialer
	TLS       *tls.Config
	Reader    *Reader
	Metrics   *wire.OpsMetrics
	Log       *slog.Logger
	Now       func() time.Time
	Freshness time.Duration

	// prevCPU holds the previous cumulative cpu.stat usage_usec
	// per instance id. The poller computes the rate as
	// (cur - prev) / (now - prevSampled) * 100. Reset to
	// (math.NaN, zeroTime) on first sight; the Reader stamps
	// CPU=Unknown for the first tick per instance.
	prevCPU map[string]cpuState
}

type cpuState struct {
	usageUsec uint64
	at        time.Time
}

// NewPoller builds a Poller with sensible defaults applied to
// zero-valued fields. Callers should set Store / Dialer / Reader
// / Metrics; the rest have safe defaults (200 ms interval, real
// time.Now, real slog default logger).
func NewPoller(store state.Store, dialer Dialer, tlsCfg *tls.Config, reader *Reader, metrics *wire.OpsMetrics, log *slog.Logger) *Poller {
	if log == nil {
		log = slog.Default()
	}
	return &Poller{
		Interval:  DefaultStatsInterval,
		Store:     store,
		Dialer:    dialer,
		TLS:       tlsCfg,
		Reader:    reader,
		Metrics:   metrics,
		Log:       log,
		Now:       time.Now,
		Freshness: DefaultFreshness,
		prevCPU:   map[string]cpuState{},
	}
}

// TickInterval returns the poller's interval. pkg/sched.Loop's
// WithInstanceStats option needs this to size its ticker.
func (p *Poller) TickInterval() time.Duration {
	if p.Interval <= 0 {
		return DefaultStatsInterval
	}
	return p.Interval
}

// Run blocks until ctx is done, ticking on the configured
// interval. The first Tick is invoked before the ticker fires so
// the first sample lands at t=0 (time.NewTicker does not fire
// immediately; this is a documented correction to the heartbeat
// loop's "first sample at t=Interval" behaviour, see issue #120).
func (p *Poller) Run(ctx context.Context) error {
	if err := p.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
		// First-tick failure is logged but does not abort the
		// loop — partial sweeps are still useful, and the next
		// tick has a fresh chance.
		p.Log.Warn("instance stats first tick failed", "err", err)
	}
	t := time.NewTicker(p.TickInterval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := p.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
				p.Log.Warn("instance stats tick failed", "err", err)
			}
		}
	}
}

// Tick performs one full sweep: list active compute nodes, list
// live instances, dial each node fresh, decode Stats, build
// InstanceStat rows, replace the Reader and the metrics. Per-node
// dial failures are logged + recorded on the per-node error
// counter; they do not abort the sweep.
func (p *Poller) Tick(ctx context.Context) error {
	started := p.now()
	nodes, err := p.Store.ActiveComputeNodes(ctx)
	if err != nil {
		return err
	}
	instances, err := p.Store.ListAllInstances(ctx)
	if err != nil {
		return err
	}
	// Group instances by node for the join.
	byNode := make(map[string][]state.Instance, len(nodes))
	for _, in := range instances {
		if in.NodeID == "" {
			continue
		}
		byNode[in.NodeID] = append(byNode[in.NodeID], in)
	}
	rows := make([]InstanceStat, 0, len(instances))
	rolled := make([]wire.InstanceStatRow, 0, len(instances))
	for _, node := range nodes {
		nodeRows, nodeRolled := p.tickNode(ctx, node, byNode[node.ID])
		rows = append(rows, nodeRows...)
		rolled = append(rolled, nodeRolled...)
	}
	// Replace is atomic; readers see either the previous snapshot
	// or the next, never a torn mix.
	p.Reader.Replace(rows)
	// Metrics rollup: max CPU / sum RSS / sum inflight per
	// (app, node). The wire side collapses NaN for absent
	// values; instancestats passes NaN through so the rollup
	// excludes them.
	if p.Metrics != nil {
		p.Metrics.ReplaceInstanceStats(rolled, p.now().Sub(started))
	}
	return nil
}

// tickNode dials one node fresh, calls Stats, and decodes the
// result into InstanceStat rows + wire rollup rows. On dial
// failure it logs, increments the per-node error counter, and
// returns empty slices — the caller continues to the next node.
func (p *Poller) tickNode(ctx context.Context, node state.ComputeNode, siblings []state.Instance) ([]InstanceStat, []wire.InstanceStatRow) {
	if p.Dialer == nil {
		return nil, nil
	}
	vmm, err := p.Dialer.Dial(ctx, node.TargetURL, p.TLS)
	if err != nil {
		p.Log.Warn("instance stats dial failed", "node_id", node.ID, "err", err)
		if p.Metrics != nil {
			p.Metrics.InstanceStatsPartialError(node.ID)
		}
		return nil, nil
	}
	defer func() { _ = vmm.Close() }()
	snap, err := vmm.Stats(ctx)
	if err != nil {
		p.Log.Warn("instance stats vmm.Stats failed", "node_id", node.ID, "err", err)
		if p.Metrics != nil {
			p.Metrics.InstanceStatsPartialError(node.ID)
		}
		return nil, nil
	}
	// Index durable sibling state by instance id for the join.
	// The poller uses state.Instance.LastRequestAt as the
	// fallback for LastRequestAt when the wire is zero (PR-A
	// only; PR-B will populate wire from ActivityTracker).
	sibByID := make(map[string]state.Instance, len(siblings))
	for _, in := range siblings {
		sibByID[in.ID] = in
	}
	now := p.now()
	rows := make([]InstanceStat, 0, len(snap.Instances))
	rolled := make([]wire.InstanceStatRow, 0, len(snap.Instances))
	for _, in := range snap.Instances {
		if in.InstanceID == "" {
			continue
		}
		durable, ok := sibByID[in.InstanceID]
		if !ok {
			// Wire reported an instance we have no
			// state for (e.g. concurrent destroy). Skip
			// rather than publish a row with empty
			// AppID — that would silently land in the
			// rollup with the wrong (app, node) tuple.
			continue
		}
		row := InstanceStat{
			InstanceID:       in.InstanceID,
			NodeID:           node.ID,
			AppID:            durable.AppID,
			InflightRequests: in.InflightRequests,
			CPU:              Unknown,
			RSS:              Unknown,
			SampledAt:        now,
		}
		wireRow := wire.InstanceStatRow{
			AppID:            durable.AppID,
			NodeID:           node.ID,
			InflightRequests: in.InflightRequests,
			CPUPct:           math.NaN(),
			RSSMB:            math.NaN(),
		}
		// CPU: cumulative counter; produce a rate only on the
		// second+ sample. A regression in usage_usec or a new
		// cgroup forces a baseline reset (Unknown for one
		// tick).
		if in.CPUPct != nil {
			// The wire is DoubleValue: the wrapper is
			// nil when absent, populated when present.
			// vmmd emits CPUPct as a *float64 read by the
			// Stats handler from cgroupstats.Sample
			// (PR-A wire) or a future client-side rate
			// (post-#170 followup). Today the schedd
			// side receives CPUPct=nil because vmmd does
			// not populate it yet. We respect the
			// contract: nil → Unknown, non-nil → Valid.
			wireRow.CPUPct = *in.CPUPct
			row.CPUPct = *in.CPUPct
			row.CPU = Valid
		}
		// RSS: wire sends *int64. nil → Unknown; non-nil →
		// convert bytes → MiB.
		if in.ResidentBytes != nil {
			mib := float64(*in.ResidentBytes) / float64(1024*1024)
			wireRow.RSSMB = mib
			row.RSSMB = mib
			row.RSS = Valid
		}
		// LastRequestAt: prefer wire (PR-B will populate
		// from ActivityTracker); fall back to durable
		// state.Instance.LastRequestAt when the wire is
		// zero.
		switch {
		case !in.LastRequestAt.IsZero():
			row.LastRequestAt = in.LastRequestAt
		case !durable.LastRequestAt.IsZero():
			row.LastRequestAt = durable.LastRequestAt
		}
		rows = append(rows, row)
		rolled = append(rolled, wireRow)
	}
	// Apply per-instance CPU delta math on top of the wire's
	// already-encoded CPUPct. The wire CPUPct today is unused
	// (vmmd doesn't populate it), but the poller is the
	// canonical place to compute rates from the cumulative
	// counter once vmmd starts emitting it. For now the
	// prevCPU map stays empty; the poller will start using
	// it the moment vmmd emits cumulative usage_usec on the
	// wire.
	//
	// (The cgroupstats.Reader returns cumulative
	// usage_usec; the Stats handler currently emits a nil
	// CPUPct wrapper, but the handler change to read the
	// cumulative and let the poller do the rate is a one-line
	// edit in PR-B's stats.go extraction. The poller is
	// already shaped for it.)
	_ = p.prevCPU
	return rows, rolled
}

// now returns the poller's wall clock. Defaults to time.Now so
// tests can inject a fake clock via Poller.Now.
func (p *Poller) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}
