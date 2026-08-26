// Package main — readiness.go constructs the vmmd-side
// /readyz probe (issue #571 PR-A2). Three signals:
//
//   - /dev/kvm openable: vmmd is the only root daemon, and it
//     cannot serve traffic if the host's KVM device is missing
//     (the cgroup v2 + jailer pair also require this). Failing
//     here is a hard error — vmmd will not start.
//   - firecracker binary on PATH: same shape as the production
//     binary path (exec.LookPath(fcvm.FirecrackerBin)). Failing
//     here is a deploy-bug, not a transient — the operator should
//     re-install firecracker.
//   - gRPC listener bound: flips true once deps.listen() returns
//     a usable net.Listener. vmmdgrpc.Server.Stats and the
//     schedd -> vmmd wake dial share this listener; /readyz
//     cannot return 200 before the listener is up.
//
// Why three signals (and not one binary "everything OK"):
// /readyz surfaces the failing reason in the body so an
// operator reading the panel can see at a glance which component
// is wedged. Single-bit /readyz hides the cause; the §12
// "vmmd readiness" dashboard pairs /readyz body with the
// reason string for triage.
package main

import (
	"errors"
	"os"
	"os/exec"
	"sync"

	"github.com/onebox-faas/faas/pkg/fcvm"
	"github.com/onebox-faas/faas/pkg/wire"
)

// devKVM is the host KVM device path vmmd requires to spawn
// Firecracker microVMs (spec §4.4 — vmmd is the only root
// component). The string is a constant (no env override) because
// a misconfiguration here is always a host-level bug.
const devKVM = "/dev/kvm"

// BuildReadinessProbe constructs the vmmd /readyz probe + the
// grpcBoundSignal the main goroutine flips immediately before
// gsrv.Serve(lis) inside the serve goroutine. Returns the probe
// and the bound-flipper.
//
// Construction-time probe values are evaluated immediately:
// /dev/kvm is opened (closed on success), and `firecracker` is
// looked up via exec.LookPath. A failing check flips its
// signal to false with the reason as the body. A passing check
// starts the signal at true so /readyz returns 200 unless the
// gRPC bind fails later (the only runtime-flipping signal).
//
// PR #1091 review Finding 5: the gRPC bound signal MUST flip
// inside the serve goroutine, immediately before
// gsrv.Serve(lis). Flipping it earlier (right after
// deps.listen() returns) leaves a ~90-line window where the
// daemon reports ready but no gRPC server is actually running —
// any panic during that window (cpuCache/netCache construction,
// impl.Register, /metrics endpoint setup, …) leaves the
// readiness probe stuck at "ready" while /readyz is scraped
// with no listener.
func BuildReadinessProbe() (*wire.ReadyzProbe, *grpcBoundSignal) {
	p := &wire.ReadyzProbe{}
	p.RegisterSignal(kvmOpenableSignal(), nil)
	p.RegisterSignal(fcBinarySignal(), nil)
	bound := &grpcBoundSignal{}
	p.RegisterSignal(bound.Signal(), nil)
	return p, bound
}

// kvmOpenableSignal returns a ReadySignal reporting whether
// /dev/kvm can be opened RDWR. The syscall is the same one the
// jailer does per-VM; doing it once at boot surfaces a
// misconfigured host (no KVM module, wrong device permissions)
// before the first wake arrives.
func kvmOpenableSignal() *wire.ReadySignal {
	s := &wire.ReadySignal{}
	f, err := os.OpenFile(devKVM, os.O_RDWR, 0)
	if err != nil {
		s.Set(false, "kvm open failed: "+err.Error())
		return s
	}
	if cerr := f.Close(); cerr != nil {
		s.Set(false, "kvm close failed: "+cerr.Error())
		return s
	}
	s.Set(true, "")
	return s
}

// fcBinarySignal returns a ReadySignal reporting whether the
// firecracker binary is on PATH. The check mirrors
// fcvm.DetectFirecrackerVersion's exec.CommandContext call
// against fcvm.FirecrackerBin. A failure here is a deploy bug
// (firecracker not installed or not on $PATH), not a transient.
func fcBinarySignal() *wire.ReadySignal {
	s := &wire.ReadySignal{}
	if _, err := exec.LookPath(fcvm.FirecrackerBin); err != nil {
		var msg string
		if errors.Is(err, exec.ErrNotFound) {
			msg = "firecracker not on PATH"
		} else {
			msg = "firecracker lookup failed: " + err.Error()
		}
		s.Set(false, msg)
		return s
	}
	s.Set(true, "")
	return s
}

// grpcBoundSignal is a flip-from-outside signal: the vmmd
// main.go calls MarkBound() immediately before gsrv.Serve(lis)
// inside the serve goroutine. Before that flip the probe
// reports "grpc not yet bound"; after the flip it reports
// ready.
//
// MarkBound is guarded by sync.Once so a panic during the
// intervening ~90 lines of vmmd setup (BuildReadinessProbe
// through impl.Register) cannot leave the signal wedged in
// the not-bound state across a retry, and so a second call
// from any future second serve-goroutine is a no-op. PR
// #1091 review Finding 5: previously MarkBound was called
// right after deps.listen() — between the listen and the
// actual gsrv.Serve call there are ~90 lines of vmmd setup
// (cpuCache, netCache, activityTracker, gsrv, impl, Register,
// optional /metrics endpoint). A panic or early-return during
// that window would leave /readyz reporting ready even though
// no gRPC server was actually running.
type grpcBoundSignal struct {
	sig  *wire.ReadySignal
	once sync.Once
}

// Signal returns the underlying *wire.ReadySignal for
// ReadyzProbe.RegisterSignal. Idempotent — multiple calls
// return the same signal.
func (g *grpcBoundSignal) Signal() *wire.ReadySignal {
	if g.sig == nil {
		g.sig = &wire.ReadySignal{}
		g.sig.Set(false, "grpc not yet bound")
	}
	return g.sig
}

// MarkBound flips the gRPC bound signal to ready. Called once
// during boot, immediately before gsrv.Serve(lis) inside the
// serve goroutine. After MarkBound, /readyz returns 200 iff
// kvm + fc-binary are also ready. Idempotent — repeated calls
// are no-ops.
func (g *grpcBoundSignal) MarkBound() {
	g.once.Do(func() {
		g.Signal().Set(true, "")
	})
}
