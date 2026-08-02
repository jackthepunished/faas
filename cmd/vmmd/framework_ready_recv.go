//go:build linux

// Host-side DGRAM recv loop for the framework-ready signal (issue
// #470 / PR #470-FU-B). The guest-init proxy (see
// guest/init/framework_ready_proxy_linux.go) dials CID=2
// (VMADDR_CID_HOST) port 1027 with a DGRAM datagram; this loop
// binds the same port on CID=2 and parses each receipt,
// resolving the source instance via the per-DGRAM peer CID
// (each Firecracker guest has a unique CID derived from
// Lease.Slot, see pkg/fcvm.GuestVsockCID).
//
// The wire shape (mirrored from
// guest/init/framework_ready_proxy_linux.go):
//
//   [1B type=0x01][optional 4B BE uint32 warmup_ms][NUL][runtime]
//
// The host strips the NUL-terminated runtime and uses the
// preceding 4 bytes (if present) as the warmup_ms duration.
// Type != 0x01 is dropped with a Warn (forward-compatible with
// a future type=0x02 "framework_idle").
//
// Concurrency: one goroutine reads the DGRAM fd. Each receipt
// is parsed and dispatched to the Manager synchronously. A
// misframed datagram is warn-logged and dropped; the loop never
// crashes on a bad peer.
package main

import (
	"encoding/binary"
	"fmt"
	"log/slog"

	"golang.org/x/sys/unix"

	"github.com/onebox-faas/faas/pkg/fcvm"
)

// VsockFrameworkReadyHostPort mirrors the guest-side
// VsockFrameworkReadyPort (issue #470 / PR #470-FU-B). The host
// binds VMADDR_CID_HOST=2 on this port; the guest-init proxy
// dials VMADDR_CID_HOST=2 on the same port. Must match on both
// sides. (guest/init/framework_ready_proxy_linux.go defines the
// guest-side constant `VsockFrameworkReadyPort`; cmd/vmmd does
// not import guest/init so the constant is duplicated here.)
const VsockFrameworkReadyHostPort uint32 = 1027

// VsockFrameworkReadyHostTypeReady is the discriminator byte for
// the "ready" message type. Future-compatibility: the host
// drops unknown types at Warn so a future type=0x02 ("idle")
// doesn't break the wire.
const VsockFrameworkReadyHostTypeReady byte = 0x01

// frameworkReadyMaxDatagram is the upper bound on the DGRAM
// body the host will accept. The guest-side wire is at most 5
// bytes (1B type + 4B BE uint32 warmup_ms + NUL) plus the
// runtime string (≤ 32 bytes — bounded by the guest runner id
// set {node22, node24, python312, python313, go124}); 64 is a
// comfortable future-proof margin.
const frameworkReadyMaxDatagram = 64

// FrameworkReadyReceiver is the host-side DGRAM listener. It
// owns the bound AF_VSOCK DGRAM socket and the read loop. The
// receiver is held by the vmmd main loop and torn down on
// context cancellation.
type FrameworkReadyReceiver struct {
	fd  int
	log *slog.Logger
	mgr *fcvm.Manager
}

// StartFrameworkReadyReceiver binds the host-side DGRAM
// listener on CID=2:VsockFrameworkReadyHostPort and spawns the
// read loop. Returns an error if the bind fails (which means
// vmmd is running on a host without AF_VSOCK — the host kernel
// doesn't have vsock loaded, or the vmmd binary is missing
// CAP_NET_RAW). The error is fatal at the cmd main() level —
// the framework-ready receipt is required for the warm-tier
// path, so the cmd path aborts if it can't come up.
//
// The Manager is the destination for every receipt. The
// receiver stores a pointer (not a value) so a Manager
// reinstalled by the cmd main loop after a config reload is
// reflected without restarting the listener.
func StartFrameworkReadyReceiver(log *slog.Logger, mgr *fcvm.Manager) (*FrameworkReadyReceiver, error) {
	if log == nil {
		log = slog.Default()
	}
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("framework_ready DGRAM socket: %w", err)
	}
	addr := &unix.SockaddrVM{CID: unix.VMADDR_CID_HOST, Port: VsockFrameworkReadyHostPort}
	if err := unix.Bind(fd, addr); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("framework_ready DGRAM bind port %d: %w", VsockFrameworkReadyHostPort, err)
	}
	r := &FrameworkReadyReceiver{fd: fd, log: log, mgr: mgr}
	go r.loop()
	log.Info("framework_ready receiver started", "vsock_host_port", VsockFrameworkReadyHostPort)
	return r, nil
}

// Close releases the DGRAM socket. Safe to call multiple times.
func (r *FrameworkReadyReceiver) Close() {
	if r == nil || r.fd < 0 {
		return
	}
	_ = unix.Close(r.fd)
	r.fd = -1
}

// loop reads datagrams in a tight loop. Each receipt is parsed
// and dispatched to the Manager. The loop terminates when the
// fd is closed (Close) or the kernel returns a syscall error
// (typically the VM exiting — the kernel may close the vsock
// proxy on the host side).
func (r *FrameworkReadyReceiver) loop() {
	buf := make([]byte, frameworkReadyMaxDatagram)
	for {
		if r.fd < 0 {
			return
		}
		n, from, err := unix.Recvfrom(r.fd, buf, 0)
		if err != nil {
			r.log.Debug("framework_ready recv loop ended", "err", err)
			return
		}
		sa, ok := from.(*unix.SockaddrVM)
		if !ok {
			r.log.Warn("framework_ready non-vsock peer", "from", from)
			continue
		}
		msg, perr := parseFrameworkReadyDatagram(buf[:n])
		if perr != nil {
			r.log.Warn("framework_ready parse", "err", perr, "len", n, "peer_cid", sa.CID)
			continue
		}
		// Resolve the peer CID → instance id via the live map.
		// The Manager owns the CID↔instance join (it knows each
		// instance's Lease.Slot which derives the CID via
		// pkg/fcvm.GuestVsockCID).
		instance, lookupErr := r.mgr.InstanceByCID(sa.CID)
		if lookupErr != nil {
			// Expected during instance churn (a DGRAM racing
			// a wake-park cycle). Log at Debug.
			r.log.Debug("framework_ready for unknown CID",
				"peer_cid", sa.CID, "warmup_ms", msg.WarmupMs,
				"runtime", msg.Runtime)
			continue
		}
		// Stamps the per-instance `framework_ready_at` clock
		// and observes the warmup histogram.
		stamped, appID, runtime, merr := r.mgr.MarkInstanceFrameworkReady(nil, instance, msg.WarmupMs)
		if merr != nil {
			r.log.Warn("framework_ready manager call", "err", merr)
			continue
		}
		if !stamped {
			r.log.Debug("framework_ready manager found no live instance",
				"instance", instance, "peer_cid", sa.CID)
			continue
		}
		// Runtime label is the runner id from the wire
		// (e.g. "node22"). The Manager's histogram already
		// stamped under (runtime, appID).
		_ = appID
		_ = runtime
	}
}

// parseFrameworkReadyDatagram parses one DGRAM body into the
// (warmup_ms, runtime) pair. The instance id is NOT on the wire
// — the host resolves it from the DGRAM peer CID instead.
func parseFrameworkReadyDatagram(b []byte) (struct {
	WarmupMs int64
	Runtime  string
}, error) {
	type out struct {
		WarmupMs int64
		Runtime  string
	}
	if len(b) == 0 {
		return out{}, fmt.Errorf("empty body")
	}
	if b[0] != VsockFrameworkReadyHostTypeReady {
		return out{}, fmt.Errorf("unknown msg sub-type 0x%02x", b[0])
	}
	rest := b[1:]
	var warmup int64
	var runtime string
	if len(rest) >= 4 {
		warmup = int64(binary.BigEndian.Uint32(rest[:4]))
		rest = rest[4:]
	}
	// rest is now [NUL][runtime string]. The NUL is the
	// boundary marker the proxy inserted; the runtime tail
	// follows.
	if idx := indexNUL(rest); idx >= 0 {
		runtime = string(rest[idx+1:])
	}
	return out{WarmupMs: warmup, Runtime: runtime}, nil
}

func indexNUL(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return -1
}
