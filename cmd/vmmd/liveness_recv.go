//go:build linux

// Host-side liveness probe loop (issue #554 / ADR-078). The
// guest-init listener (guest/init/liveness_linux.go) binds AF_VSOCK
// STREAM port 1028 inside the VM; this file dials that port on
// every Period, runs the framed probe request, and translates the
// 4-class outcome into the consecutive-failure counter. After
// ConsecutiveFailures non-2xx (or timeout / conn-refused) responses
// the goroutine calls Manager.ReportLivenessFailed, which the
// schedd-side relay drains into Engine.DestroyForLivenessFailure.
//
// The wire envelope mirrors ADR-022's resume hook + the
// guest-init listener:
//
//	4-byte big-endian msg-type   = guest.VsockLivenessMsgProbe (10)
//	4-byte big-endian body-len
//	N-byte JSON body             = {"path":"/healthz", "timeout_ms":2000}
//
//	(responding)
//	4-byte big-endian msg-type   = guest.VsockLivenessMsgAck (11)
//	4-byte big-endian body-len
//	N-byte JSON body             = {"status":200, "err":""}
//
// Lifecycle: one livenessProbeLoop goroutine per instance. The
// Manager owns the lifecycle (start on BringUp, stop on
// DestroyForLivenessFailure or Park). The goroutine exits on
// ctx cancellation (cmd vmmd shutdown) or on a fatal vsock
// error; the per-iteration tick is timer-driven so a single
// missed probe doesn't compound with the next.
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/onebox-faas/faas/pkg/fcvm"
)

// VsockLivenessHostPort mirrors the guest-side
// guest.VsockLivenessPort (= 1028). Must match on both sides.
// Duplicated here because cmd/vmmd does not import guest/init
// (one-way layering).
const VsockLivenessHostPort uint32 = 1028

// livenessRequestBody is the JSON body the host ships to the guest.
// Mirrors guest/init/livenessReq.
type livenessRequestBody struct {
	Path      string `json:"path"`
	TimeoutMs int    `json:"timeout_ms"`
}

// livenessResponseBody is the JSON body the guest ships back.
// Mirrors guest/init/livenessResp.
type livenessResponseBody struct {
	Status int    `json:"status"`
	Err    string `json:"err"`
}

// livenessProbeOutcomes is the closed set the vmmd
// poll goroutine tracks in the vmmd_guest_liveness_probe_seconds
// histogram. Each value is the Prometheus label suffix.
const (
	livenessOutcomeOK         = "ok"
	livenessOutcomeNon200     = "non_200"
	livenessOutcomeTimeout    = "timeout"
	livenessOutcomeConnRefused = "conn_refused"
	livenessOutcomeConnErr    = "conn_err"
)

// livenessProbeConfig is the per-instance configuration the
// schedd-side produces (issue #554 / ADR-078). Path is the runner's
// HTTP path (must start with "/"); Period / ConsecutiveFailures /
// CooldownSeconds are the per-deployment overrides that fall back
// to the per-plan defaults (Hobby/Pro/Scale → 5s / 3 / 60s).
//
// The schedd resolver reads the deployment row's
// `override_liveness_probe` JSONB and merges with the parent app's
// plan defaults before producing this struct. cmd/vmmd wires the
// resolved struct via WithLivenessProbes at manager construction.
type livenessProbeConfig struct {
	Path                string
	PeriodSeconds       int
	ConsecutiveFailures int
	CooldownSeconds     int
	// IdleResetOnDestroy is the user-confirmed choice (issue #554
	// §Implementation notes): the per-instance idle timer should
	// reset on liveness-destroy so the reaper grace restarts on
	// the cold-boot instance, not on the wedged one. Surfaced as
	// a boolean here so the schedd side can encode the policy
	// without us hard-coding it on the vmmd side.
	IdleResetOnDestroy bool
}

// livenessProbeLoop is the per-instance poll goroutine. The
// Manager owns a map[*livenessProbeLoop]cancelFunc so a Park /
// Destroy race can stop the loop without waiting for the next
// tick.
type livenessProbeLoop struct {
	instance  string
	cfg       livenessProbeConfig
	cid       uint32
	mgr       *fcvm.Manager
	log       *slog.Logger
	cancel    context.CancelFunc
	count     int  // current consecutive-failure count
	lastReset time.Time
	// probeFn is the test seam: production code uses dialAndProbe
	// (real AF_VSOCK), tests inject a stub that returns the
	// closed-set outcome string ("ok", "non_200", "timeout",
	// "conn_refused", "conn_err"). Default = nil → runOne uses
	// the real dialAndProbe.
	probeFn func(ctx context.Context, timeoutMs int) (string, int)
}

// runLivenessProbeLoop is the entry point. Blocks until ctx is
// done. The poll cadence is cfg.PeriodSeconds (default 5s); the
// per-probe timeout is min(cfg.PeriodSeconds * 1000, 5000)ms —
// the hard ceiling matches the guest-init's
// VsockLivenessHardTimeoutMs.
//
// On every non-2xx / timeout / conn-refused response the count
// increments; on every 2xx it resets to 0. When count reaches
// cfg.ConsecutiveFailures the loop calls Manager.ReportLivenessFailed
// and exits (the schedd side will paint the instance stopped, and
// the Manager will cancel the loop via the destruction path).
func (l *livenessProbeLoop) run(ctx context.Context) {
	if l.cfg.PeriodSeconds <= 0 {
		// The plan didn't enable liveness for this instance.
		return
	}
	tick := time.NewTicker(time.Duration(l.cfg.PeriodSeconds) * time.Second)
	defer tick.Stop()
	timeoutMs := l.cfg.PeriodSeconds * 1000
	if timeoutMs > 5000 {
		timeoutMs = 5000
	}
	if timeoutMs < 1000 {
		timeoutMs = 1000
	}
	// Single-shot probe on entry so a Steady-State VM doesn't
	// have to wait PeriodSeconds to validate the liveness path
	// is wired. Failures here still count toward the counter.
	l.runOne(ctx, timeoutMs)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			l.runOne(ctx, timeoutMs)
		}
	}
}

// runOne executes one probe. The 4-class outcome is folded into the
// consecutive-failure counter + the vmmd_guest_liveness_probe_seconds
// histogram. The 5s deadline is tighter than the period so a stuck
// probe doesn't compound (the next tick is honoured regardless).
func (l *livenessProbeLoop) runOne(ctx context.Context, timeoutMs int) {
	start := time.Now()
	var outcome string
	var status int
	if l.probeFn != nil {
		outcome, status = l.probeFn(ctx, timeoutMs)
	} else {
		outcome, status = l.dialAndProbe(ctx, timeoutMs)
	}
	elapsed := time.Since(start).Seconds()
	l.mgr.ObserveLivenessProbe(outcome, elapsed)
	switch outcome {
	case livenessOutcomeOK:
		// Reset on the first 2xx (AC #2 — intermittent failures
		// must not produce oscillation).
		if l.count > 0 {
			l.log.Debug("liveness probe reset",
				"instance", l.instance, "prev_count", l.count)
		}
		l.count = 0
		l.mgr.SetLivenessConsecutiveFailures(l.instance, 0)
	default:
		l.count++
		l.mgr.SetLivenessConsecutiveFailures(l.instance, l.count)
		if l.count >= l.cfg.ConsecutiveFailures {
			// Mirror the reason the run-time classifies
			// into the relay so the schedd side audit
			// event names the cluster correctly.
			reason := "liveness_n_consecutive"
			if outcome == livenessOutcomeTimeout {
				reason = "liveness_timeout"
			} else if outcome == livenessOutcomeConnRefused {
				reason = "liveness_conn_refused"
			} else if outcome == livenessOutcomeConnErr {
				reason = "liveness_conn_err"
			} else if outcome == livenessOutcomeNon200 {
				reason = "liveness_non_200"
			}
			l.mgr.ReportLivenessFailed(ctx, l.instance, reason)
			// Exit the loop — schedd's Engine.DestroyForLivenessFailure
			// will Park / destroy the instance, and the
			// Manager will cancel this loop via the
			// teardown path. Don't re-arm the counter.
			return
		}
	}
	_ = status
}

// livenessProbeDialTimeout is the absolute cap on the dial+read.
// 5s matches the guest-init's VsockLivenessHardTimeoutMs ceiling.
const livenessProbeDialTimeout = 5 * time.Second

// dialAndProbe opens an AF_VSOCK STREAM connection to the per-VM
// CID on VsockLivenessHostPort, ships the probe body, and returns
// the (outcome, status) pair. The classification mirrors the four
// classes the guest-init reports — see guest/init/liveness_linux.go.
func (l *livenessProbeLoop) dialAndProbe(ctx context.Context, timeoutMs int) (string, int) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return livenessOutcomeConnErr, 0
	}
	defer func() { _ = unix.Close(fd) }()
	dialCtx, cancel := context.WithTimeout(ctx, livenessProbeDialTimeout)
	defer cancel()
	addr := &unix.SockaddrVM{CID: l.cid, Port: VsockLivenessHostPort}
	// unix.Connect is non-blocking on the AF_VSOCK STREAM socket
	// the kernel returns immediately with EINPROGRESS; we wrap
	// in a deadline-driven polling loop. The connect itself
	// blocks until the guest-init's listener accepts OR the
	// deadline fires (the livenessProbeDialTimeout cap).
	connectDone := make(chan error, 1)
	go func() {
		connectDone <- unix.Connect(fd, addr)
	}()
	select {
	case <-ctx.Done():
		return livenessOutcomeConnErr, 0
	case <-dialCtx.Done():
		return livenessOutcomeConnRefused, 0
	case err := <-connectDone:
		if err != nil {
			// ECONNREFUSED is the expected signal when the
			// guest-init's listener is NOT up yet (a hot
			// rebuild before boot). The failure counter
			// increments — the CooldownSeconds gate in the
			// schedd-side window protects against a cold
			// boot noise signature.
			return livenessOutcomeConnRefused, 0
		}
	}
	body, err := json.Marshal(livenessRequestBody{
		Path:      l.cfg.Path,
		TimeoutMs: timeoutMs,
	})
	if err != nil {
		return livenessOutcomeConnErr, 0
	}
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[:4], 10) // guest.VsockLivenessMsgProbe
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(body)))
	if err := writeAll(fd, hdr[:]); err != nil {
		return livenessOutcomeConnErr, 0
	}
	if err := writeAll(fd, body); err != nil {
		return livenessOutcomeConnErr, 0
	}
	// Read the response envelope. Deadline is the same
	// livenessProbeDialTimeout cap; we use a deadline-tracked
	// read loop instead of SetReadDeadline so the guest's
	// VsockLivenessDefaultTimeoutMs (2s) is the natural
	// timeout surface.
	var respHdr [8]byte
	if err := readAll(fd, respHdr[:], livenessProbeDialTimeout); err != nil {
		return livenessOutcomeTimeout, 0
	}
	mt := binary.BigEndian.Uint32(respHdr[:4])
	if mt != 11 {
		// guest.VsockLivenessMsgAck — wire-shape regression
		// if it doesn't match.
		return livenessOutcomeConnErr, 0
	}
	bodyLen := binary.BigEndian.Uint32(respHdr[4:8])
	if bodyLen == 0 || bodyLen > 4096 {
		return livenessOutcomeConnErr, 0
	}
	respBody := make([]byte, bodyLen)
	if err := readAll(fd, respBody, livenessProbeDialTimeout); err != nil {
		return livenessOutcomeTimeout, 0
	}
	var resp livenessResponseBody
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return livenessOutcomeConnErr, 0
	}
	if resp.Err != "" {
		// The guest-init itself classified the failure —
		// fold the wire-side reason into the 4-class outcome
		// so the histogram stays the source of truth.
		switch resp.Err {
		case "timeout":
			return livenessOutcomeTimeout, resp.Status
		case "conn_refused":
			return livenessOutcomeConnRefused, resp.Status
		case "runner_not_ready":
			return livenessOutcomeConnRefused, resp.Status
		default:
			return livenessOutcomeConnErr, resp.Status
		}
	}
	if resp.Status >= 200 && resp.Status < 300 {
		return livenessOutcomeOK, resp.Status
	}
	return livenessOutcomeNon200, resp.Status
}

// writeAll is the unix-style write-loop helper. Mirrors the
// guest-init's readFull. Both live here so the failure surface is
// symmetric — if this helper changes, the guest-side readFull gets
// a corresponding bump.
func writeAll(fd int, b []byte) error {
	for len(b) > 0 {
		n, err := unix.Write(fd, b)
		if n > 0 {
			b = b[n:]
		}
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return err
		}
		if n == 0 {
			return fmt.Errorf("vsock write: 0 bytes")
		}
	}
	return nil
}

// readAll is the unix-style read-loop helper. The deadline is
// wall-clock; we honour ctx cancellation in addition so a
// parent-ctx cancel pre-empts the deadline.
func readAll(fd int, b []byte, deadline time.Duration) error {
	ch := make(chan error, 1)
	go func() {
		for len(b) > 0 {
			n, err := unix.Read(fd, b)
			if n > 0 {
				b = b[n:]
			}
			if err != nil {
				if err == unix.EINTR {
					continue
				}
				ch <- err
				return
			}
			if n == 0 {
				ch <- fmt.Errorf("vsock read: 0 bytes (EOF)")
				return
			}
		}
		ch <- nil
	}()
	select {
	case err := <-ch:
		return err
	case <-time.After(deadline):
		return fmt.Errorf("vsock read deadline %s", deadline)
	}
}

// livenessRegistry maps an instance id to the cancelFunc that
// stops its probe loop. The Manager owns one of these per daemon
// (lifetime = vmmd process). The Park path cancels the entry so
// the loop exits on the next ctx.Done — within one tick, not at
// the next probe.
//
// goroutine-safe: the Park path can call cancel() while the
// runOne is blocked in dialAndProbe; the loop's ctx.Done
// plumbs the cancellation through.
type livenessRegistry struct {
	mu      sync.Mutex
	loops   map[string]context.CancelFunc
}

// newLivenessRegistry constructs an empty registry.
func newLivenessRegistry() *livenessRegistry {
	return &livenessRegistry{loops: make(map[string]context.CancelFunc)}
}

// startLivenessLoop spins up a new probe loop for the instance.
// The context is the parent vmmd ctx; the loop inherits it via
// WithCancel so the cmd-level shutdown kills the loop in addition
// to the per-instance Park.
func (r *livenessRegistry) start(ctx context.Context, parent context.Context, loop *livenessProbeLoop) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.loops[loop.instance]; exists {
		// Defensive: a re-registration for the same
		// instance (e.g. a BringUp racing a Park) cancels
		// the prior loop. The schedd side guarantees the
		// instance id is unique per live tile.
		r.loops[loop.instance]()
	}
	loopCtx, cancel := context.WithCancel(parent)
	loop.cancel = cancel
	r.loops[loop.instance] = cancel
	go func() {
		loop.run(loopCtx)
		r.mu.Lock()
		delete(r.loops, loop.instance)
		r.mu.Unlock()
	}()
}

// cancelLoop stops the loop for the instance. Idempotent.
func (r *livenessRegistry) cancelLoop(instance string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cancel, ok := r.loops[instance]; ok {
		cancel()
		delete(r.loops, instance)
	}
}
