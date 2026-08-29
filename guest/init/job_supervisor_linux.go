// Package main — job-task supervisor linux implementation
// (issue #1184 Workstream A / ADR-099).
//
// This file implements the runJob entry point + signal handling +
// vsock DGRAM shipping for job-task VMs. Mirrors characterize_linux.go's
// pattern (dial host CID 2, write framed envelope, await 1-byte ack)
// but with SOCK_DGRAM (the wire-format discriminator that lets the
// job-exit port number overlap with characterize's).
//
// Flow (single iteration, no restart loop — jobs run once to
// terminal):
//
//  1. Load /etc/faas/job.json (JobManifest).
//  2. Start a wall-clock timer at task_timeout_s (T).
//  3. Build merged env (systemEnv ⊕ job.Env ⊕ jobEnvBaseline).
//  4. Fork+Exec syscall.Exec(command[0], command, env) — replaces
//     init so signals land directly on the customer process
//     (proper signal handling: SIGTERM triggers Go's default
//     handler, not guest-init's).
//  5. Capture exit_code + signal via syscall.WaitStatus after
//     exec returns. Wait doesn't actually return on a successful
//     syscall.Exec — exec replaces the process image — so
//     catching the exit means we either fell back to os/exec or
//     exec failed and we re-entered the loop. In practice the
//     only way to land here is via os/exec.Cmd so we can ship
//     the vsock DGRAM before poweroff.
//  6. Write JobExitPayload to vsock DGRAM (port 1026, msg_type 4).
//  7. poweroff -f.

package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// RunJob is the entry point the boot dispatcher calls when
// decideMode picks modeJob. It is total — on any internal error
// it still attempts to ship a vsock DGRAM (so the host's
// HandleJobExit sees a terminal transition, not a hang), then
// powers off. The VM is single-shot; there's no restart loop.
//
// Lifecycle:
//   - returns nil on clean exec + DGRAM ack
//   - returns error only if the vsock DGRAM itself fails; in
//     that case we poweroff anyway so the host doesn't see a
//     runaway VM. The error is logged at WARN so the reaper's
//     later sweep of the task slot surfaces the gap.
//
// Order matters:
//  1. loadJobManifest (must succeed; missing manifest = not a
//     job VM and the boot dispatcher shouldn't have called us)
//  2. startTimeoutWatcher (sets up the SIGTERM→30s grace→SIGKILL
//     timer that fires if the customer's command outlives
//     task_timeout_s)
//  3. execCommand (syscall.Exec, replacing the supervisor's
//     process image so signals reach the customer directly)
//  4. The path after exec.Command returning means exec FAILED
//     and we're still in the supervisor; we map the error to
//     an exit envelope and ship it.
//
// Note on syscall.Exec semantics: on success, syscall.Exec NEVER
// returns. So a code path after exec.Command(...) is the
// "exec failed" branch. We use os/exec.Cmd (not syscall.Exec) for
// the failure path so the wait status is inspectable, then call
// exec.Command(command[0], command[1:]...) again — but only as a
// last resort because we need to ship the DGRAM. If even the
// os/exec path fails (e.g. command missing in the image), we
// log + ship a "failed" envelope + poweroff.
func RunJob(log *slog.Logger) error {
	manifest, err := loadJobManifest()
	if err != nil {
		log.Error("runJob: load manifest", "err", err)
		return shipAndPoweroff(JobExitPayload{
			ExitCode:   127, // POSIX "command not found" sentinel
			ErrorClass: "infra",
			LeaseToken: "",
		}, log)
	}
	if len(manifest.Command) == 0 {
		return shipAndPoweroff(JobExitPayload{
			ExitCode:   126, // POSIX "command found but not executable" sentinel
			ErrorClass: "infra",
			LeaseToken: manifest.LeaseToken,
		}, log)
	}

	// Start the wall-clock timeout watcher. SIGTERM at the
	// deadline, SIGKILL after 30s grace. Independent goroutine
	// so the customer's signal handlers (or absence thereof)
	// don't affect our cleanup. ctx cancel on clean exit kills
	// the watcher.
	if manifest.TaskTimeoutSec > 0 {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		startTimeoutWatcher(ctx, manifest.TaskTimeoutSec, os.Getpid())
	}

	// Build merged env (systemEnv ⊕ job.Env ⊕ jobEnvBaseline).
	env := buildEnvForJob(*manifest)

	// Try syscall.Exec first — it replaces the process image
	// so the customer's signal handlers receive SIGTERM
	// directly, not guest-init's. On Linux this is the
	// canonical pattern for "I'm just a thin wrapper".
	if err := unix.Exec(manifest.Command[0], manifest.Command, env); err != nil {
		// exec failed — likely ENOENT or EACCES. Fall back to
		// os/exec.Cmd so we can capture the WaitStatus. If even
		// os/exec fails (e.g. ENOENT), ship an "infra" envelope
		// with exit_code=127.
		log.Warn("runJob: syscall.Exec failed, falling back to os/exec",
			"err", err, "command", manifest.Command[0])
		return runViaOSExec(*manifest, env, log)
	}
	// Unreachable: unix.Exec replaces the process on success.
	return nil
}

// runViaOSExec is the fallback path when syscall.Exec fails
// (e.g. the command path isn't absolute, or the binary doesn't
// have the execute bit). It shells out via os/exec.Cmd so we
// can inspect the WaitStatus and ship a meaningful DGRAM.
//
// Returns the exit envelope via shipAndPoweroff.
func runViaOSExec(m JobManifest, env []string, log *slog.Logger) error {
	cmd := exec.Command(m.Command[0], m.Command[1:]...)
	cmd.Env = env
	// Stdout / Stderr → null so the customer's command's output
	// doesn't get mixed into guest-init's log ring. Logs are
	// captured via the vsock DGRAM (future M-extra work — M8
	// ships with stdout discarded; logs land in pkg/fcvm/logbuf
	// once we add a log forwarder).
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		log.Error("runJob: os/exec start", "err", err, "command", m.Command[0])
		return shipAndPoweroff(JobExitPayload{
			ExitCode:   127,
			ErrorClass: "infra",
			LeaseToken: m.LeaseToken,
		}, log)
	}
	waitErr := cmd.Wait()
	exitCode := int32(-1)
	var signal int32
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
				if ws.Signaled() {
					signal = int32(ws.Signal())
					exitCode = 128 + signal
				} else {
					exitCode = int32(ws.ExitStatus())
				}
			}
		}
	} else {
		exitCode = 0
	}
	return shipAndPoweroff(JobExitPayload{
		ExitCode:           exitCode,
		ErrorClass:         mapExitToErrorClass(exitCode, signal),
		Signal:             signal,
		FinishedAtUnixNano: time.Now().UnixNano(),
		LeaseToken:         m.LeaseToken,
	}, log)
}

// startTimeoutWatcher fires SIGTERM at the customer's command
// once task_timeout_s elapses, then SIGKILL after 30s grace if
// the customer installed a SIGTERM handler that ignores the signal.
// The goroutine exits when ctx is cancelled (which runJob's
// caller does via defer).
//
// On Linux, SIGTERM lands directly on the customer process
// because syscall.Exec replaced the supervisor. If the customer
// process is in a process group of its own, SIGTERM goes to the
// supervisor's PID (which is now the customer's) — same outcome.
//
// Sends SIGKILL to PID once 30s past the SIGTERM if the process
// is still alive. sendSignal is best-effort; an ESRCH means the
// process already exited and the watcher can exit cleanly.
//
// CR-6 / code-review #6: the previous implementation pre-started
// both the deadline AND the 30s grace timer at function entry.
// When task_timeout_s > 30s (the common case — Hobby=300s, Pro
// =1800s), the grace timer fired at t=30s and its value sat in
// the buffered channel. After SIGTERM at the deadline, the
// goroutine did `<-grace.C` which returned immediately (the value
// was already queued), and SIGKILL was sent with effectively
// zero grace — a customer that needed the full 30s to drain
// in-flight work was killed mid-shutdown. Fix: don't arm the
// grace timer until after SIGTERM is delivered, so the 30s window
// starts at the SIGTERM moment.
func startTimeoutWatcher(ctx context.Context, taskTimeoutSec int, pid int) {
	if taskTimeoutSec <= 0 {
		return
	}
	deadline := time.NewTimer(time.Duration(taskTimeoutSec) * time.Second)
	defer deadline.Stop()
	stopDeadline := func() {
		if !deadline.Stop() {
			select {
			case <-deadline.C:
			default:
			}
		}
	}
	go func() {
		// Clean exit: ctx cancel stops the deadline timer + the
		// goroutine. (No grace timer exists yet at this point.)
		select {
		case <-ctx.Done():
			stopDeadline()
			return
		case <-deadline.C:
			// SIGTERM at the deadline. Now arm the 30s grace
			// timer — its 30s window starts at THIS instant,
			// not at function entry.
			_ = unix.Kill(pid, unix.SIGTERM)
		}
		grace := time.NewTimer(30 * time.Second)
		defer grace.Stop()
		select {
		case <-ctx.Done():
			if !grace.Stop() {
				select {
				case <-grace.C:
				default:
				}
			}
			return
		case <-grace.C:
			// 30s grace elapsed; SIGKILL the customer.
			_ = unix.Kill(pid, unix.SIGKILL)
		}
	}()
}

// loadJobManifest reads + decodes /etc/faas/job.json from the
// post-pivot root fs.
//
// Returns (nil, nil) when the file is missing — decideMode
// already proved the file existed before calling RunJob, so
// missing here means a race (vmmd didn't stage the file in time)
// and the supervisor should treat it as infra error.
// A present-but-malformed file returns the decode error so a
// corrupted staging write surfaces as a panic rather than a
// silent app-mode fallback.
func loadJobManifest() (*JobManifest, error) {
	// open from cwd-relative path — guest-init has pivot_root'd
	// into /, so a relative path resolves against the merged
	// overlay root.
	f, err := os.Open(filepath.Join("/", jobManifestPath))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", jobManifestPath, err)
	}
	defer func() { _ = f.Close() }()
	var m JobManifest
	if err := json.NewDecoder(f).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode %s: %w", jobManifestPath, err)
	}
	return &m, nil
}

// shipAndPoweroff writes the JobExitPayload envelope to the
// host via vsock DGRAM (port 1026, msg_type 4), then powers
// off the VM. Always called from a terminal path (no return).
//
// The DGRAM retry matches characterize's STREAM retry (3
// attempts, 100/250/500ms backoff) — same host-side listener
// availability window (post-boot, pre-poweroff).
//
// poweroff -f is the same fast-shutdown command characterize
// uses; the kernel ACPI handler does the rest.
func shipAndPoweroff(payload JobExitPayload, log *slog.Logger) error {
	shipExitEnvelope(payload, log)
	// Best-effort poweroff. If poweroff fails (e.g. the binary
	// is missing in the customer's image), the reaper on the
	// host side takes over after the lease TTL elapses.
	_ = exec.Command("poweroff", "-f").Run()
	return nil
}

// shipExitEnvelope is the inner DGRAM-writer, factored out so
// unit tests can drive it with a mock vsock target. Production
// callers always use shipAndPoweroff.
//
// Format: [4B BE msg_type][4B BE body_len][N B JSON].
// Matches pkg/fcvm.WaitJobExit's parse (vmm.go).
//
// Uses SOCK_DGRAM (NOT SOCK_STREAM) — the discriminator that
// lets the port number overlap with characterize's STREAM.
// Connect+Sendto behave identically for DGRAM at this layer;
// we use Sendto because it's the canonical DGRAM path and
// doesn't require a Connect step.
func shipExitEnvelope(payload JobExitPayload, log *slog.Logger) {
	body, err := json.Marshal(payload)
	if err != nil {
		log.Error("job-exit marshal", "err", err)
		return
	}
	if len(body) > VsockJobExitMaxBody {
		body = body[:VsockJobExitMaxBody]
	}
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], VsockJobExitMsgType)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(body)))
	frame := append(hdr[:], body...)

	addr := &unix.SockaddrVM{
		CID:  unix.VMADDR_CID_HOST,
		Port: VsockJobExitPort,
	}
	const attempts = 3
	backoffs := []time.Duration{100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond}
	for i := 0; i < attempts; i++ {
		sock, sockErr := unix.Socket(unix.AF_VSOCK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
		if sockErr != nil {
			log.Warn("job-exit vsock socket", "err", sockErr, "attempt", i)
			if i < attempts-1 {
				time.Sleep(backoffs[i])
			}
			continue
		}
		// Per-socket send deadline (1.5s; same as characterize).
		setSockTimeout(sock, unix.SO_SNDTIMEO, 1500*time.Millisecond)
		if wErr := unix.Sendto(sock, frame, 0, addr); wErr != nil {
			log.Warn("job-exit vsock send", "err", wErr, "attempt", i)
			_ = unix.Close(sock)
			if i < attempts-1 {
				time.Sleep(backoffs[i])
			}
			continue
		}
		_ = unix.Close(sock)
		// DGRAM has no ack; the host either got it or didn't.
		// The reaper sweep picks up the lease on timeout.
		log.Info("job-exit shipped",
			"exit_code", payload.ExitCode,
			"error_class", payload.ErrorClass,
			"signal", payload.Signal,
			"attempt", i+1)
		return
	}
	log.Error("job-exit all attempts failed", "attempts", attempts)
}

// jobManifestFixturePath is a test-only fs.FS fixture path used
// by job_supervisor_linux_test.go's loadJobManifest fixture
// helper. Drift between this and jobManifestPath is a test
// failure (the test asserts the paths match).
const jobManifestFixturePath = "etc/faas/job.json"

// hasJobManifest is the boot-time check decideMode calls before
// dispatching to RunJob. Mirrors the build.json / app.json
// presence check at decideMode.
//
// Reads /etc/faas/job.json from a fs.FS so unit tests can drive
// it with testing/fstest.MapFS without touching the real root
// fs. The real boot path passes os.DirFS("/") so the path
// resolves against the merged overlay root.
//
// Returns true ONLY if the file exists AND its "kind" field is
// "job". A present file with a missing/wrong "kind" is treated
// as "not a job VM" — the same fail-soft posture
// characterize_linux.go uses for missing fields (so a future
// schema drift doesn't panic every VM).
func hasJobManifest(fsys fs.FS) bool {
	data, err := fs.ReadFile(fsys, jobManifestFixturePath)
	if err != nil {
		return false
	}
	var probe struct {
		Kind string `json:"kind"`
	}
	if jErr := json.Unmarshal(data, &probe); jErr != nil {
		return false
	}
	return probe.Kind == "job"
}
