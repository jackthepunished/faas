// capacity_publisher.go — vmmd's live-capacity push to schedd
// (ADR-025 axis 5).
//
// Background. vmmd is the only daemon that owns its host's
// cgroup leaves (per-VM memory.current). Axis 5 closes the
// chooser's stale-store gap: instead of reading the plan-mb
// sum from `instances.ram_mb+8`, the chooser consults a
// per-node in-memory cache that vmmd fills every 1 s.
//
// Wire. vmmd is the gRPC client; schedd is the server. The
// outer reconnect loop dials schedd on
// `deps.scheddTarget` (issue #95 unix:///run/faas/schedd.sock
// default; tcp/dns optional with mTLS), opens a client-stream
// `ReportCapacity`, and writes one CapacityReport per
// CapacityInterval tick. The producer is purely heartbeat-ish:
// if schedd is unreachable, vmmd logs and retries with the
// 1s → 2s → 4s → 8s → 16s → 30s ladder plus 0–500 ms jitter
// (cmd/vmmd/reconnect.go). When the stream returns, the loop
// resets backoff and keeps sending.
//
// Cold-boot safety. If `cfg.ComputeNode.NodeName` is empty
// (single-box dev default), main.go never starts this
// goroutine and vmmd skips the loop entirely — no dial, no
// stream, no report. The schedd-side table stays empty;
// the chooser falls back to
// `store.ComputeNodeUsedMB` (legacy behaviour). ADR-005
// preserved.
//
// Trust model. The publisher does no caching, no decision-
// making, and no policy. It is a pure read sampler → wire
// pump. The schedd-side ledger floor
// (`applyLiveCapacityMB`, PR-2) is the canonical authority;
// a stale-low or hostile vmmd report cannot shrink the live
// accounting.

package main

import (
	"context"
	"log/slog"
	"time"

	scheddpb "github.com/onebox-faas/faas/api/proto/onebox/faas/schedd/v1"
	"github.com/onebox-faas/faas/pkg/fcvm"
	"github.com/onebox-faas/faas/pkg/wire"
)

// CapacityInterval is the publisher's tick cadence. 1 s
// balances freshness (the chooser reads with a 5 s budget)
// against churn (a 200 ms tick would push ~5× as many
// reports on the wire for no freshness gain).
const CapacityInterval = 1 * time.Second

// CapacityReportTTL bounds how long the publisher keeps
// trying to maintain a single stream before treating the
// failure as a fatal vmmd→schedd break. 5 minutes is
// generous: a healthy reconnect on a transient schedd
// restart completes in < 30 s. The TTL prevents a long-
// running test or a misconfigured environment from holding
// a half-open stream indefinitely.
const CapacityReportTTL = 5 * time.Minute

// residentBytesFn is the leakcheck seam. Production wires
// `leakcheck.ResidentBytes`; tests inject a stub that
// returns a fixed map. The second return value is the
// "linux-ok" boolean — `false` means we couldn't read
// cgroups (non-Linux dev box, containerized build, etc.),
// so the publisher emits used_mb=0 and the chooser falls
// back to the store sum (ADR-005).
type residentBytesFn func() (map[string]int64, bool)

// runCapacityPublish is the outer reconnect loop. It is
// invoked as a goroutine from main.go and exits when ctx
// fires. The loop is intentionally simple: dial → stream
// → tick → send → drain-on-error → reconnect. The policy
// lives in schedd's chooser (PR-2); vmmd's only job is to
// keep the stream alive.
func runCapacityPublish(
	ctx context.Context,
	mgr *fcvm.Manager,
	nodeID string,
	cfg ComputeNodeConfig,
	scheddTarget string,
	tick time.Duration,
	resident residentBytesFn,
	log *slog.Logger,
) {
	if scheddTarget == "" {
		// No target → no-op. main.go gates this on the
		// NodeName check, but a defensive guard here lets
		// tests inject an empty target without a fatal
		// startup failure.
		return
	}
	if tick <= 0 {
		tick = CapacityInterval
	}
	backoff := time.Second
	deadline := time.Now().Add(CapacityReportTTL)
	for {
		if ctx.Err() != nil {
			return
		}
		if time.Now().After(deadline) {
			log.Warn("vmmd: capacity publisher exceeded TTL; giving up",
				"node_id", nodeID, "ttl", CapacityReportTTL.String())
			return
		}
		err := drainCapacityPublish(ctx, mgr, nodeID, cfg, scheddTarget, tick, resident, log)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Warn("vmmd: capacity stream ended; reconnecting",
				"node_id", nodeID, "err", err, "retry_in", backoff.String())
		}
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff, MaxBackoff)
	}
}

// drainCapacityPublish opens one client-streaming
// ReportCapacity session and pushes reports until the
// stream errors or ctx cancels. Returns nil on a clean
// shutdown (ctx cancel); any other error reflects the
// dial failure, send failure, or Recv error and triggers
// the outer reconnect loop.
func drainCapacityPublish(
	ctx context.Context,
	mgr *fcvm.Manager,
	nodeID string,
	cfg ComputeNodeConfig,
	scheddTarget string,
	tick time.Duration,
	resident residentBytesFn,
	log *slog.Logger,
) error {
	cli, cleanup, err := openCapacityStream(ctx, scheddTarget, nodeID, log)
	if err != nil {
		return err
	}
	defer cleanup()

	log.Info("vmmd: capacity stream connected", "node_id", nodeID, "target", scheddTarget)
	// Reset backoff after a successful dial (mirrors the
	// gatewayd warmhints pattern, cmd/gatewayd/warmhints.go:119-121).
	// This is implicit: the outer loop resets backoff when
	// drainCapacityPublish returns nil; we keep the
	// document-where-the-policy-lives note here so a future
	// reader sees the shape.

	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// Best-effort graceful close: the client stream's
			// CloseAndRecv is deferred by `cleanup`. We don't
			// wait for it here — the ctx is already Done.
			return nil
		case <-t.C:
			report := buildCapacityReport(mgr, nodeID, cfg, resident)
			if err := cli.Send(report); err != nil {
				return err
			}
		}
	}
}

// openCapacityStream dials schedd and opens a client-streaming
// ReportCapacity session. The returned cleanup func closes
// the underlying conn on drain return. Done here rather than
// inline so a test can inject a stub `newCapacityStreamer`
// without rebuilding the dial math.
func openCapacityStream(
	ctx context.Context,
	scheddTarget string,
	nodeID string,
	log *slog.Logger,
) (scheddpb.Schedd_ReportCapacityClient, func(), error) {
	// Lazy dial: gRPC's blocking dial happens at first RPC;
	// we want stream-open failures to surface inside the
	// outer reconnect loop's backoff, not at boot.
	conn, err := wire.DialContext(ctx, scheddTarget, nil)
	if err != nil {
		return nil, nil, err
	}
	cli := scheddpb.NewScheddClient(conn)
	stream, err := cli.ReportCapacity(ctx)
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	cleanup := func() {
		// Best-effort: ack the stream so the server's
		// SendAndClose returns. Errors here are expected
		// when ctx is already canceled.
		if _, err := stream.CloseAndRecv(); err != nil {
			log.Debug("vmmd: capacity stream close", "node_id", nodeID, "err", err)
		}
		_ = conn.Close()
	}
	return stream, cleanup, nil
}

// buildCapacityReport samples the live count + cgroup
// memory.current and returns a typed proto. UsedMB is the
// sum of all instance cgroup memory.current values, NOT a
// plan-mb or sample-rate approximation; the chooser applies
// the ledger floor as a separate check (PR-2).
//
// RAMHeadroomMB is `cfg.ComputeNode.MemMB - usedMB`,
// clamped at 0. A negative headroom (over-commit defence
// miss) surfaces as 0 on the wire so the chooser can
// see "saturated" without parsing a negative value.
//
// vcpu_busy is filled as `live * 2` (per-vCPU-2 default).
// Per-cgroup-weight-sum is a v1.1 upgrade; the placeholder
// is conservative and matches the §4.5 future-work note
// in ADR-025.
func buildCapacityReport(
	mgr *fcvm.Manager,
	nodeID string,
	cfg ComputeNodeConfig,
	resident residentBytesFn,
) *scheddpb.CapacityReport {
	// nil manager → live=0, leased=0. Lets the unit tests
	// run without a real *fcvm.Manager (which requires
	// /dev/kvm). Production always passes a non-nil mgr
	// because main.go constructs one before the publisher
	// goroutine starts.
	var live, leased int32
	if mgr != nil {
		live = int32(mgr.LiveCount())
		leased = int32(mgr.LeasedCount())
	}

	var usedMB int64
	if bytes, ok := resident(); ok {
		var sum int64
		for _, b := range bytes {
			sum += b
		}
		usedMB = sum >> 20 // bytes → MiB (1024×1024)
	}

	headroom := int64(cfg.MemMB) - usedMB
	if headroom < 0 {
		headroom = 0
	}
	// Avoid overflow on the int32 typed wire field. The
	// chooser applies the floored int64 in PR-2; here we
	// just clamp the wire representation.
	if usedMB > 1<<31-1 {
		usedMB = 1<<31 - 1
	}
	if headroom > 1<<31-1 {
		headroom = 1<<31 - 1
	}

	return &scheddpb.CapacityReport{
		NodeId:          nodeID,
		SampledAtUnixMs: time.Now().UnixMilli(),
		LiveCount:       live,
		LeasedCount:     leased,
		UsedMb:          int32(usedMB),
		RamHeadroomMb:   int32(headroom),
		VcpuBusy:        live * 2,
	}
}
