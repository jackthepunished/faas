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

	"github.com/onebox-faas/faas/pkg/fcvm"
	"github.com/onebox-faas/faas/pkg/wire"
)

// devKVM is the host KVM device path vmmd requires to spawn
// Firecracker microVMs (spec §4.4 — vmmd is the only root
// component). The string is a constant (no env override) because
// a misconfiguration here is always a host-level bug.
const devKVM = "/dev/kvm"

// BuildReadinessProbe constructs the vmmd /readyz probe + the
// grpcBoundSignal the main goroutine flips after the gRPC
// listener is bound. Returns the probe and the bound-flipper.
//
// Construction-time probe values are evaluated immediately:
// /dev/kvm is opened (closed on success), and `firecracker` is
// looked up via exec.LookPath. A failing check flips its
// signal to false with the reason as the body. A passing check
// starts the signal at true so /readyz returns 200 unless the
// gRPC bind fails later (the only runtime-flipping signal).
func BuildReadinessProbe() (*wire.ReadyzProbe, *grpcBoundSignal) {
	p := &wire.ReadyzProbe{}
	p.RegisterSignal(kvmOpenableSignal())
	p.RegisterSignal(fcBinarySignal())
	bound := &grpcBoundSignal{}
	p.RegisterSignal(bound.Signal())
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
// main.go calls MarkBound() after deps.listen() returns a
// usable net.Listener. Before that flip the probe reports
// "grpc not yet bound"; after the flip it reports ready.
//
// Not goroutine-safe — the vmmd boot path is single-threaded
// until ctx-cancel, and MarkBound is called once during boot.
// A future race-free refactor would wrap MarkBound in
// sync.Once.
type grpcBoundSignal struct {
	sig *wire.ReadySignal
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
// during boot, after deps.listen() returns. After MarkBound,
// /readyz returns 200 iff kvm + fc-binary are also ready.
func (g *grpcBoundSignal) MarkBound() {
	g.Signal().Set(true, "")
}