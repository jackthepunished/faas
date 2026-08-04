//go:build linux

// Host-side DGRAM recv loop for the framework-ready signal (issue
// #470 / PR #470-FU-B) AND the sidecar events channel
// (issue #463 / ADR-069 / ADR-071 / PR-C). The guest-init proxy
// (see guest/init/framework_ready_proxy_linux.go) dials CID=2
// (VMADDR_CID_HOST) port 1027 with a DGRAM datagram; this loop
// binds the same port on CID=2 and parses each receipt,
// resolving the source instance via the per-DGRAM peer CID
// (each Firecracker guest has a unique CID derived from
// Lease.Slot, see pkg/fcvm.GuestVsockCID).
//
// The wire shape (mirrored from
// guest/init/{framework_ready,sidecar_events}_proxy_linux.go):
//
//	[1B type=0x01][optional 4B BE uint32 warmup_ms][NUL][runtime]
//	[1B type=0x02][json envelope: sidecar_init_exit]
//	[1B type=0x03][json envelope: sidecar_restart]
//
// The host strips the NUL-terminated runtime and uses the
// preceding 4 bytes (if present) as the warmup_ms duration for
// type=0x01; for type=0x02/0x03 the remainder of the body is a
// small UTF-8 JSON envelope. Type outside the closed set {0x01,
// 0x02, 0x03} is dropped with a Warn (forward-compatible with
// future event classes).
//
// Concurrency: one goroutine reads the DGRAM fd. Each receipt
// is parsed and dispatched to the Manager synchronously. A
// misframed datagram is warn-logged and dropped; the loop never
// crashes on a bad peer.
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

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
// the "ready" message type. Issue #463 / ADR-069 / ADR-071 / PR-C
// adds two more types to the same channel: 0x02 = sidecar_init_exit,
// 0x03 = sidecar_restart. The closed set keeps the wire bounded
// — a future event class picks the next free byte and is its own
// PR.
const (
	VsockFrameworkReadyHostTypeReady    byte = 0x01
	VsockFrameworkReadyHostTypeInitExit byte = 0x02
	VsockFrameworkReadyHostTypeRestart  byte = 0x03
)

// frameworkReadyMaxDatagram is the upper bound on the DGRAM
// body the host will accept. The guest-side wire for type=0x01 is
// at most 5 bytes (1B type + 4B BE uint32 warmup_ms + NUL) plus
// the runtime string (≤ 32 bytes — bounded by the guest runner id
// set {node22, node24, python312, python313, go124}); 64 is a
// comfortable future-proof margin. Type 0x02/0x03 carry a JSON
// envelope that the guest-init proxy caps at
// guest/init::sidecarMaxDatagram = 512 bytes; we read up to
// frameworkReadyMaxDatagram for ALL types so the host bound is
// the larger of the two. The Linux vsock DGRAM max is well
// above 4 KiB on a stock kernel; 1024 is a generous future-proof
// margin that still pinpoints a runaway sender (4+ KiB frames
// from guest-init are a bug).
const frameworkReadyMaxDatagram = 1024

// FrameworkReadyReceiver is the host-side DGRAM listener. It
// owns the bound AF_VSOCK DGRAM socket and the read loop. The
// receiver is held by the vmmd main loop and torn down on
// context cancellation.
//
// fd is an atomic.Int32 (not a plain int) because Close() writes
// to it from the main path while loop() reads it on every
// recv call. Using a plain int trips `go test -race` between
// the two goroutines (CRIT-related review feedback on PR
// #470-FU-B). The zero value is meaningless; Close publishes
// the sentinel -1 to break the loop.
//
// emitter (issue #463 / ADR-069 / ADR-071 / PR-C) is the
// audit sink for the sidecar event classes (init_exit /
// restart). Set by WithSidecarEmitter after construction (the
// method lives in sidecar_events_wire.go so cmd/vmmd/main.go
// can call it on every build); nil falls back to a no-op
// emitter so the framework_ready path is unaffected by a
// missing sidecar wiring (e.g. local-dev without a
// state.Store).
type FrameworkReadyReceiver struct {
	ctx     context.Context
	fd      atomic.Int32
	log     *slog.Logger
	mgr     *fcvm.Manager
	emitter SidecarEventEmitter
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
// reflected without restarting the listener. sidecarEmitter
// is the audit sink for the sidecar event classes; nil = the
// no-op default (no audit, but the dispatch never blocks).
func StartFrameworkReadyReceiver(ctx context.Context, log *slog.Logger, mgr *fcvm.Manager) (*FrameworkReadyReceiver, error) {
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
	r := &FrameworkReadyReceiver{ctx: ctx, log: log, mgr: mgr, emitter: noopSidecarEventEmitter{}}
	r.fd.Store(int32(fd))
	go r.loop()
	log.Info("framework_ready receiver started", "vsock_host_port", VsockFrameworkReadyHostPort)
	return r, nil
}

// Close releases the DGRAM socket. Safe to call multiple times.
// Synchronises with the loop() reader via atomic.Int32.Load:
// the loop checks r.fd < 0 on every iteration and exits when
// Close publishes the sentinel.
func (r *FrameworkReadyReceiver) Close() {
	if r == nil {
		return
	}
	old := r.fd.Swap(-1)
	if old < 0 {
		return
	}
	_ = unix.Close(int(old))
}

// loop reads datagrams in a tight loop. Each receipt is parsed
// and dispatched to the Manager (type=0x01) or to the
// sidecar-event emitter (type=0x02/0x03). The loop terminates
// when the fd is closed (Close) or the kernel returns a syscall
// error (typically the VM exiting — the kernel may close the
// vsock proxy on the host side).
func (r *FrameworkReadyReceiver) loop() {
	buf := make([]byte, frameworkReadyMaxDatagram)
	for {
		// Atomic load: a concurrent Close publishes -1
		// here. The check runs on every iteration so the
		// loop exits within one recv of a Close call.
		if r.fd.Load() < 0 {
			return
		}
		n, from, err := unix.Recvfrom(int(r.fd.Load()), buf, 0)
		if err != nil {
			// EBADF is the expected terminal error when Close()
			// publishes the -1 sentinel between the inner Load
			// and the kernel entering the syscall. Log at Debug
			// to keep the Info channel clean on graceful
			// shutdown (MED-6 review feedback on PR #543).
			// Other errors (EINTR, EAGAIN under non-blocking,
			// ENOTCONN if the vsock device unloads) are also
			// terminal for this loop — keep the Debug level so
			// a noisy kernel doesn't alarm the operator.
			r.log.Debug("framework_ready recv loop ended", "err", err, "ebadf", errors.Is(err, unix.EBADF))
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
		// pkg/fcvm.GuestVsockCID). A fresh lookup on every
		// receipt keeps the loop stateless across churn.
		instance, lookupErr := r.mgr.InstanceByCID(sa.CID)
		if lookupErr != nil {
			// Expected during instance churn (a DGRAM racing
			// a wake-park cycle). Log at Debug. Closed for
			// ALL types so a sidecar_init_exit / restart
			// datagram from a guest that just parked isn't
			// a noisy Warn.
			r.log.Debug("framework_ready-scope DGRAM for unknown CID",
				"peer_cid", sa.CID, "type", msg.TypeLabel())
			continue
		}
		switch msg.Kind {
		case parseFWReadyKindOK:
			r.dispatchFrameworkReady(instance, msg.WarmupMs)
		case parseFWReadyKindInitExit:
			r.dispatchSidecarInitExit(instance, msg.InitExit)
		case parseFWReadyKindRestart:
			r.dispatchSidecarRestart(instance, msg.Restart)
		}
	}
}

// dispatchFrameworkReady (extracted from the loop body, issue
// #463 / ADR-069 / ADR-071 / PR-C): the type=0x01 dispatch path.
// Stamps the per-instance `framework_ready_at` clock and
// observes the warmup histogram. The receiver's stored ctx is
// passed so a cmd shutdown cancels the call before it returns
// to the loop. Runtime label is the runner id from the wire
// (e.g. "node22"). The Manager's histogram already stamped under
// (runtime, appID).
func (r *FrameworkReadyReceiver) dispatchFrameworkReady(instance string, warmupMs int64) {
	stamped, appID, runtime, merr := r.mgr.MarkInstanceFrameworkReady(r.ctx, instance, warmupMs)
	if merr != nil {
		r.log.Warn("framework_ready manager call", "err", merr)
		return
	}
	if !stamped {
		r.log.Debug("framework_ready manager found no live instance",
			"instance", instance)
		return
	}
	_ = appID
	_ = runtime
}

// dispatchSidecarInitExit (issue #463 / ADR-069 / ADR-071 /
// PR-C §3): the type=0x02 dispatch path. Resolves the
// (instance → appID) join via Manager and forwards the wire
// envelope to the sidecar-event emitter. A failed lookup
// (the instance parked between the guest's send and our
// recv) is logged at Debug, not Warn — the audit is
// best-effort.
func (r *FrameworkReadyReceiver) dispatchSidecarInitExit(instance string, wire sidecarInitExitWire) {
	if wire.Status != "init_ok" && wire.Status != "init_failed" {
		r.log.Warn("sidecar_init_exit unknown status", "instance", instance, "status", wire.Status)
		return
	}
	appID, perr := r.mgr.InstanceAppID(instance)
	if perr != nil {
		r.log.Debug("sidecar_init_exit unknown instance", "instance", instance, "err", perr)
		return
	}
	r.emitter.EmitSidecarInitExit(r.ctx, instance, appID, "" /* wakeID not on wire — see pkg/events.SidecarInitExit's struct doc */, wire)
	if wire.Status == "init_failed" {
		// AC #1 surface: a failed init is a hard fail, and
		// the operator-visible audit must show
		// failure_class: user_error. The audit row is keyed
		// on (deployment_id, sidecar) so the deployments UI
		// can group init-side failures across the fleet.
		r.log.Error("sidecar_init_exit init_failed (AC #1)",
			"instance", instance, "app_id", appID, "sidecar", wire.Sidecar,
			"exit_code", wire.ExitCode, "duration_ms", wire.DurationMs)
	}
}

// dispatchSidecarRestart (issue #463 / ADR-069 / ADR-071 /
// PR-C §4): the type=0x03 dispatch path. Same join as init_exit;
// PR-C §4 increments the vmmd_sidecar_restart_total counter
// in the emitter (the counter lives on wire.OpsMetrics which the
// emitter wraps). Wired here so the §3 commit ships both
// dispatch arms; the §4 commit only needs to add the
// guest-init Supervisor.OnCrash emit hook to actually drive
// type=0x03.
func (r *FrameworkReadyReceiver) dispatchSidecarRestart(instance string, wire sidecarRestartWire) {
	appID, perr := r.mgr.InstanceAppID(instance)
	if perr != nil {
		r.log.Debug("sidecar_restart unknown instance", "instance", instance, "err", perr)
		return
	}
	r.emitter.EmitSidecarRestart(r.ctx, instance, appID, "" /* wakeID not on wire — see pkg/events.SidecarRestart's struct doc */, wire)
}

// parseFWKind is the discriminator for
// parseFrameworkReadyDatagram (issue #463 / ADR-069 /
// ADR-071 / PR-C). Closed set: OK for type=0x01, InitExit
// for type=0x02, Restart for type=0x03. A future type=0x04
// adds its own enum value here.
type parseFWKind uint8

const (
	parseFWReadyKindUnknown parseFWKind = iota
	parseFWReadyKindOK
	parseFWReadyKindInitExit
	parseFWReadyKindRestart
)

// parseFWReadyMsg is the typed return of
// parseFrameworkReadyDatagram. Type=0x01 fills WarmupMs +
// Kind; type=0x02/0x03 fill the matching envelope. The
// instance id is NOT on the wire — the host resolves it from
// the DGRAM peer CID.
type parseFWReadyMsg struct {
	Kind     parseFWKind
	WarmupMs int64
	// runtime carries the runner id (e.g. "node22") for type=0x01 only.
	Runtime  string
	InitExit sidecarInitExitWire
	Restart  sidecarRestartWire
}

// TypeLabel returns a human-readable label of the discriminated
// type, used only for diagnostic logs (Debug level).
func (m parseFWReadyMsg) TypeLabel() string {
	switch m.Kind {
	case parseFWReadyKindOK:
		return fmt.Sprintf("framework_ready(0x%02x)", VsockFrameworkReadyHostTypeReady)
	case parseFWReadyKindInitExit:
		return fmt.Sprintf("sidecar_init_exit(0x%02x)", VsockFrameworkReadyHostTypeInitExit)
	case parseFWReadyKindRestart:
		return fmt.Sprintf("sidecar_restart(0x%02x)", VsockFrameworkReadyHostTypeRestart)
	default:
		return "unknown"
	}
}

// parseFrameworkReadyDatagram parses one DGRAM body into the
// typed parseFWReadyMsg union. Closed type set: 0x01
// (framework_ready), 0x02 (sidecar_init_exit), 0x03
// (sidecar_restart). The instance id is NOT on the wire — the
// host resolves it from the DGRAM peer CID instead.
func parseFrameworkReadyDatagram(b []byte) (parseFWReadyMsg, error) {
	var msg parseFWReadyMsg
	if len(b) == 0 {
		return msg, fmt.Errorf("empty body")
	}
	rest := b[1:]
	switch b[0] {
	case VsockFrameworkReadyHostTypeReady:
		msg.Kind = parseFWReadyKindOK
		var warmup int64
		if len(rest) >= 4 {
			warmup = int64(binary.BigEndian.Uint32(rest[:4]))
			rest = rest[4:]
		}
		// rest is now [NUL][runtime string]. The NUL is
		// the boundary marker the proxy inserted; the
		// runtime tail follows.
		if idx := indexNUL(rest); idx >= 0 {
			msg.Runtime = string(rest[idx+1:])
		}
		msg.WarmupMs = warmup
	case VsockFrameworkReadyHostTypeInitExit:
		msg.Kind = parseFWReadyKindInitExit
		// The body after the type byte is a UTF-8 JSON
		// envelope. json.Unmarshal tolerates trailing
		// whitespace; it does NOT tolerate trailing bytes
		// past the JSON value, so the guest proxy's
		// canonical shape (a single JSON object, no
		// trailing junk) is what we rely on.
		if err := json.Unmarshal(rest, &msg.InitExit); err != nil {
			return msg, fmt.Errorf("sidecar_init_exit: %w", err)
		}
	case VsockFrameworkReadyHostTypeRestart:
		msg.Kind = parseFWReadyKindRestart
		if err := json.Unmarshal(rest, &msg.Restart); err != nil {
			return msg, fmt.Errorf("sidecar_restart: %w", err)
		}
	default:
		return msg, fmt.Errorf("unknown msg sub-type 0x%02x", b[0])
	}
	return msg, nil
}

func indexNUL(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return -1
}
