//go:build linux

// Unix-socket proxy the guest's runners (node22, python312, etc.)
// use to ship the per-runner "framework ready" signal to the host
// (issue #470 / PR #470-FU-B).
//
// Why a proxy?
//
// The runner process inside the guest doesn't speak AF_VSOCK
// directly — it runs in the customer's app namespace and only
// has access to the localhost unix domain socket at
// /run/guest-init/framework-ready.sock. The proxy bridges:
// runner → AF_UNIX stream → guest-init → AF_VSOCK DGRAM → host.
//
// The proxy is started in boot() BEFORE the supervisor starts the
// runners, so the first request can't race the proxy coming up;
// see the wiring in main_linux.go.
//
// Wire (runner → proxy → vsock DGRAM):
//
//   proxy connect sends a single line: "<runtime> <warmup_ms>\n"
//
//   proxy replies with one of:
//     "ok\n"   ← receipt accepted (forwarded to vsock)
//
//   proxy forwards to vsock DGRAM (port 1027, msg_type 4) with
//   the body:
//     [1B type=0x01][optional 4B BE uint32 warmup_ms][NUL][runtime]
//
// The runner side is intentionally narrow: one line of text. The
// complex shape (msg_type byte, NUL-terminated runtime, optional
// 4-byte BE uint32) is the proxy's responsibility so the runner
// shim (guest/runners/internal/framework_ready.go) stays portable
// across Python, Node, and Go.
package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// FrameworkReadyProxyPath is the localhost unix-socket path the
// runners connect to. Mounted on the guest's writable layer
// (/run is tmpfs in guest-init) so the directory exists at
// boot. The path is the same for every guest — the proxy is
// per-microVM, and the bind is local to the guest's network
// namespace, so two guests can't clash.
const FrameworkReadyProxyPath = "/run/guest-init/framework-ready.sock"

// FrameworkReadyProxyDir is the directory the proxy socket lives
// in. Created by startFrameworkReadyProxy (idempotent — MkdirAll
// on an existing dir is a no-op).
const FrameworkReadyProxyDir = "/run/guest-init"

// FrameworkReadyProxyMode is the file mode of the proxy socket
// (0660 — owner+group read/write). The riders that connect (the
// runner processes) are forked by the guest-init supervisor and
// inherit the same uid; the runner binary needs to be in the
// guest-init group for the connect to succeed. This is the same
// shape the guest-init /run/guest-init/control.sock uses for the
// supervisor-handshake protocol.
const FrameworkReadyProxyMode = 0o660

// startFrameworkReadyProxy brings up the unix-socket listener
// and the vsock DGRAM sender (one shared outbound socket) and
// spawns the accept loop in the background. Returns are tolerated
// at the boot() caller — the platform contract is "no signal".
//
// Lifecycle: caller (boot) invokes this once after
// listenFrameworkReady, before supervise() starts the runners.
// The accept loop runs forever on a single goroutine; per-
// connection work is in its own goroutine (a slow runner should
// not pin the accept queue).
func startFrameworkReadyProxy(log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}

	// 1. Build /run/guest-init. MkdirAll is idempotent on an
	// existing dir. We don't stat-check before opening the
	// socket because the unlink below covers the "stale socket"
	// case.
	if err := os.MkdirAll(FrameworkReadyProxyDir, 0o755); err != nil {
		return fmt.Errorf("proxy mkdir: %w", err)
	}
	// Stale socket from a previous boot of the same guest (the
	// guest-init PID-1 re-execs on rare configuration-only
	// reboots). Unlink unconditionally; the open below will
	// fail loud if a real socket is bound at the path.
	if err := os.Remove(FrameworkReadyProxyPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("proxy unlink %s: %w", FrameworkReadyProxyPath, err)
	}

	// 2. AF_UNIX stream listener on the proxy socket.
	addr, err := net.ResolveUnixAddr("unix", FrameworkReadyProxyPath)
	if err != nil {
		return fmt.Errorf("proxy resolve: %w", err)
	}
	ln, err := net.ListenUnix("unix", addr)
	if err != nil {
		return fmt.Errorf("proxy listen: %w", err)
	}
	// Mode 0660 — owner+group rw. The runner shim binaries are
	// forked by the supervisor and inherit the guest-init uid;
	// the runtime group is created by imaged at image build
	// time so the runner can connect.
	if err := os.Chmod(FrameworkReadyProxyPath, FrameworkReadyProxyMode); err != nil {
		_ = ln.Close()
		return fmt.Errorf("proxy chmod: %w", err)
	}

	// 3. AF_VSOCK DGRAM outbound socket. Bound on
	// VMADDR_CID_ANY:VsockFrameworkReadyPort (the same port the
	// listener read loop in listen_framework_ready_linux.go
	// binds on). DGRAM is connectionless so sendto to
	// VMADDR_CID_HOST:VsockFrameworkReadyPort is the only
	// outbound direction.
	vsock, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("proxy vsock socket: %w", err)
	}
	if err := unix.Bind(vsock, &unix.SockaddrVM{
		CID:  unix.VMADDR_CID_ANY,
		Port: VsockFrameworkReadyPort,
	}); err != nil {
		_ = unix.Close(vsock)
		_ = ln.Close()
		return fmt.Errorf("proxy vsock bind %d: %w", VsockFrameworkReadyPort, err)
	}

	// 4. Accept loop. Each connection is one runner's first
	// non-5xx response — the runner shim opens a fresh
	// connection per signal (sync.Once on the runner side
	// guarantees at-most-one signal per wake).
	go func() {
		defer func() { _ = ln.Close() }()
		defer func() { _ = unix.Close(vsock) }()
		for {
			conn, err := ln.AcceptUnix()
			if err != nil {
				log.Debug("proxy accept ended", "err", err)
				return
			}
			go handleFrameworkReadyConn(conn, vsock, log)
		}
	}()

	log.Info("framework_ready proxy started",
		"socket", FrameworkReadyProxyPath,
		"vsock_port", VsockFrameworkReadyPort)
	return nil
}

// handleFrameworkReadyConn reads one line from the runner
// ("<runtime> <warmup_ms>\n"), frames it for vsock DGRAM, and
// sends to VMADDR_CID_HOST:VsockFrameworkReadyPort. Closes the
// connection on return regardless of error. Writes back "ok\n"
// on success or "err <reason>\n" on failure so the runner shim
// can log without re-implementing the dgram framing.
func handleFrameworkReadyConn(conn *net.UnixConn, vsock int, log *slog.Logger) {
	defer func() { _ = conn.Close() }()

	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		_, _ = conn.Write([]byte("err read_line\n"))
		log.Warn("proxy read line", "err", err)
		return
	}
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 1 || len(fields) > 2 {
		_, _ = conn.Write([]byte("err format: '<runtime> [warmup_ms]'\n"))
		return
	}
	runtime := fields[0]
	var warmupMs int64
	if len(fields) == 2 {
		w, perr := strconv.ParseInt(fields[1], 10, 64)
		if perr != nil {
			_, _ = conn.Write([]byte("err parse warmup_ms\n"))
			return
		}
		warmupMs = w
	}

	// Frame: [1B type=0x01][optional 4B BE uint32 warmup_ms]
	// [NUL][runtime]. The host's recv loop (cmd/vmmd/manager.go)
	// parses the type byte, then the optional warmup_ms, then
	// strips the trailing NUL and reads the runtime string.
	var body []byte
	if warmupMs > 0 {
		buf := make([]byte, 1+4+1+len(runtime))
		buf[0] = 0x01
		binary.BigEndian.PutUint32(buf[1:5], uint32(warmupMs))
		buf[5] = 0
		copy(buf[6:], runtime)
		body = buf
	} else {
		buf := make([]byte, 1+1+len(runtime))
		buf[0] = 0x01
		buf[1] = 0
		copy(buf[2:], runtime)
		body = buf
	}

	dst := &unix.SockaddrVM{
		CID:  unix.VMADDR_CID_HOST,
		Port: VsockFrameworkReadyPort,
	}
	if _, err := unix.SendmsgN(vsock, body, nil, dst, 0); err != nil {
		_, _ = conn.Write([]byte("err send_vsock\n"))
		log.Warn("proxy send vsock", "err", err, "runtime", runtime)
		return
	}
	_, _ = conn.Write([]byte("ok\n"))
	log.Debug("framework_ready forwarded",
		"runtime", runtime, "warmup_ms", warmupMs)
}

// _ keeps filepath referenced on platforms where net and other
// imports shift around. The proxy file is linux-only; this is a
// belt-and-suspenders reference for the import block.
var _ = filepath.Join
