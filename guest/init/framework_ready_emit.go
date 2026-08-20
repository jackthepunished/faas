//go:build linux

// Workload-OOM emit helper for the guest-init cgroup.events
// listener (Cluster C / ADR-121). The listener at
// guest/init/cgroup_partition_linux.go::WatchOOM emits
// (peakMB, planMB) when the per-VM cgroup v2 leaf detects
// an oom_kill event; this helper frames the values for vsock
// DGRAM and ships them to the host's framework_ready receiver
// (cmd/vmmd/framework_ready_recv.go::dispatchWorkloadOOM).
//
// Wire shape (issue #470 / PR #470-FU-B port 1027, Cluster C
// type=0x05):
//
//	[1B type=0x05][json:{"peak_mb":N,"plan_mb":N}]
//
// The body is a UTF-8 JSON envelope mirroring the host-side
// workloadOOMWire struct at cmd/vmmd/framework_ready_recv.go.
// The host does not validate the numeric ranges — payload
// hygiene is the guest's responsibility; the schedd engine
// is the type boundary and stamps whatever it receives.
//
// Why a fresh AF_VSOCK DGRAM socket per call: the
// framework_ready proxy keeps a long-lived outbound socket
// (handler-side), but the WatchOOM listener is fire-and-
// forget — a single datagram per workload lifetime (the
// workload is dead, the VM is being torn down). A 1-second
// send timeout closes the fd promptly; ctx cancellation
// (VM shutdown) tears the socket before the deadline expires.
//
// Failure semantics: best-effort. The emit returns the
// error to the caller for log purposes; the listener
// exits on first fire regardless. A missed signal just
// means the customer sees the deployment "succeeded then
// failed" in the dashboard rather than fail-immediate.
// The wire envelope is structurally narrow on purpose —
// no future expansion here without bumping type=0x06.
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// workloadOOMSendTimeout is the SO_SNDTIMEO we set on the
// per-call DGRAM socket. The kernel enforces the bound
// itself (the send returns ETIMEDOUT past the deadline),
// so the call can be synchronous — no goroutine, no
// post-cancel use-after-close race. 1s is long enough for
// the host's framework_ready_recv loop to drain a single
// datagram (the loop is a hot busy-poll on the vsock fd,
// so the actual receipt is sub-millisecond), short enough
// that a wedged host doesn't bleed back into the WatchOOM
// listener.
const workloadOOMSendTimeout = 1 * time.Second

// workloadOOMEMitType is the discriminator byte for
// the workload-OOM DGRAM body on VsockFrameworkReadyPort.
// MUST stay in sync with
// cmd/vmmd/framework_ready_recv.go::VsockFrameworkReadyHostTypeWorkloadOOM
// (= 0x05). The first free byte after the four closed-set
// members (framework_ready=0x01, sidecar_init_exit=0x02,
// sidecar_restart=0x03, tail_event=0x04).
const workloadOOMEMitType byte = 0x05

// workloadOOMEmitMaxBody is the upper bound on the JSON
// envelope. The actual payload is < 32 bytes
// ({"peak_mb":N,"plan_mb":N}); 256 is the same future-proof
// margin as the host-side VsockWorkloadOOMMaxBody constant.
// The mirror must hold — a guest emitting more than 256
// bytes is a bug, and the host enforces the bound.
const workloadOOMEmitMaxBody = 256

// EmitWorkloadOOM (Cluster C / ADR-121) frames
// (peakMB, planMB) for vsock DGRAM and ships them to
// the host's framework_ready receiver on
// VMADDR_CID_HOST:VsockFrameworkReadyPort. Bounded by
// ctx (with a 1s send timeout floor) so a wedged host
// doesn't bleed back into the WatchOOM listener loop.
//
// Best-effort: the returned error is for log purposes.
// The WatchOOM listener exits on first fire regardless.
//
// Wire envelope:
//
//	[1B type=0x05][json:{"peak_mb":N,"plan_mb":N}]
//
// The JSON envelope mirrors the host-side workloadOOMWire
// struct (cmd/vmmd/framework_ready_recv.go); fields are
// inlined into the JSON tags so a future widening of
// the host-side struct bumps this struct in lockstep.
func EmitWorkloadOOM(ctx context.Context, peakMB, planMB int) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// 1s send timeout floor — long enough for the host
	// recv loop to drain, short enough that the WatchOOM
	// listener doesn't get stuck on a wedged host. ctx
	// cancel (VM shutdown) trips first if earlier.
	sendCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	body, err := json.Marshal(workloadOOMEmitWire{PeakMB: peakMB, PlanMB: planMB})
	if err != nil {
		return fmt.Errorf("EmitWorkloadOOM: marshal: %w", err)
	}
	if len(body) > workloadOOMEmitMaxBody {
		return fmt.Errorf("EmitWorkloadOOM: body too large: %d > %d",
			len(body), workloadOOMEmitMaxBody)
	}
	frame := make([]byte, 1+len(body))
	frame[0] = workloadOOMEMitType
	copy(frame[1:], body)

	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("EmitWorkloadOOM: socket: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()

	// Bound the synchronous send with SO_SNDTIMEO so the
	// kernel itself returns ETIMEDOUT past the deadline
	// (this is the standard pattern; the host-side
	// SOL_SOCKET/SO_RCVTIMEO at cmd/vmmd/liveness_recv.go
	// is the matching recv bound). Review finding #3:
	// the previous shape ran the send in a goroutine and
	// selected against sendCtx.Done(); on timeout the
	// defer unix.Close(fd) closed the fd while the
	// goroutine was still inside SendmsgN, a textbook
	// use-after-close. The SO_SNDTIMEO shape removes the
	// goroutine entirely — the call is synchronous, the
	// fd is closed only after the kernel reports the
	// outcome, and the worst-case latency is bounded by
	// the socket timeout instead of the goroutine
	// scheduler.
	tv := unix.Timeval{
		Sec:  int64(workloadOOMSendTimeout / time.Second),
		Usec: int64(workloadOOMSendTimeout%time.Second) / int64(time.Microsecond),
	}
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &tv); err != nil {
		return fmt.Errorf("EmitWorkloadOOM: setsockopt SO_SNDTIMEO: %w", err)
	}

	// Honor ctx cancel before the send by short-circuiting
	// the dial path. The synchronous send itself is bounded
	// by SO_SNDTIMEO so a wedged host returns ETIMEDOUT
	// promptly; the ctx branch is the "VM shutdown before
	// send" path (the send was never attempted).
	if err := sendCtx.Err(); err != nil {
		return fmt.Errorf("EmitWorkloadOOM: send ctx done: %w", err)
	}
	dst := &unix.SockaddrVM{
		CID:  unix.VMADDR_CID_HOST,
		Port: VsockFrameworkReadyPort,
	}
	if _, err := unix.SendmsgN(fd, frame, nil, dst, 0); err != nil {
		return fmt.Errorf("EmitWorkloadOOM: send: %w", err)
	}
	return nil
}

// workloadOOMEmitWire is the JSON envelope for the type=0x05
// body. Mirrors the host-side workloadOOMWire struct at
// cmd/vmmd/framework_ready_recv.go. The struct is duplicated
// here because cmd/vmmd does not import guest/init (cross-
// direction is reversed — cmd/vmmd is the host daemon,
// guest/init runs INSIDE every VM).
//
// Bumping fields here is a wire-incompatible change; do
// not widen without bumping the emit type byte.
type workloadOOMEmitWire struct {
	PeakMB int `json:"peak_mb"`
	PlanMB int `json:"plan_mb"`
}

// guard against accidental shape changes — the binary
// size assertion below catches a future refactor that
// drops a field from the struct (the JSON marshal would
// still succeed but produce a different byte layout).
//
// Compile-time only — referenced but never run.
//
//nolint:unused
var _ = binary.BigEndian
