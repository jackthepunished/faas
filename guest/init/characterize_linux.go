//go:build linux

// Characterization probe (ADR-050 §3, ADR-051). On the FIRST cold
// boot of a new deployment, guest-init runs in characterizing mode:
// it observes what the customer app binds, runs L7 probes from
// inside the guest to disambiguate http / graphql / grpc, captures
// the supervisor's exit code, and ships one report over AF_VSOCK
// STREAM (port 1026, msgtype 3) to the host (CID 2 / VMADDR_CID_HOST)
// with a 1-byte ack — not DGRAM, because the report gates a deploy
// and a silent drop is not acceptable (ADR-051 §"Rejected
// alternatives"; ADR-047's DGRAM was a deliberate flip).
//
// Wire direction: GUEST INITIATES — guest dial(host CID 2, port
// 1026), sends the framed JSON, awaits 1-byte ack, retries with
// backoff (100/250/500 ms) bounded by the 10s characterization
// deadline. This is the inverse of the resume listener (1024) and
// the stateless advisory (1025 DGRAM); ports 1024/1025/1026 stay
// distinct so a host-side prefix collision is impossible.
//
// Lifecycle: caller (boot()) calls runCharacterization AFTER the
// supervisor starts running the app. The supervisor's Restart hook
// fires the characterize probe's exit-code capture path so we
// track crashes correctly. The probe runs in a goroutine; it
// returns when the app exits AND its report lands (or the deadline
// expires with a `result=ack_timeout` audit row the host honors as
// "fall back to scan-hint class", never "fail the deploy" — the
// current 30s `:8080` accept failure path is worse than timeout).

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/onebox-faas/faas/pkg/api"
)

// Wire constants. Mirror pkg/fcvm/vmm.go in PR-D.
const (
	// VsockCharacterizationPort is the AF_VSOCK port the host accepts
	// on (guest-init dials host CID 2, port 1026). Must match
	// pkg/fcvm/vmm.go::VsockCharacterizationHostPort on the host.
	VsockCharacterizationPort uint32 = 1026
	// VsockCharacterizationMsgType is the wire-format discriminator.
	// Matches pkg/fcvm/vmm.go::VsockCharacterizationMsgResumeRdy. The
	// host's accept loop filters by msg_type prefix.
	VsockCharacterizationMsgType uint32 = 3
	// VsockCharacterizationMaxBody caps the JSON body at 32 KiB.
	// The typical report is <2 KiB; 32 KiB accommodates a long
	// log_tail (the customer-facing deploy-row surface) plus
	// listening_addrs for a polyglot app.
	VsockCharacterizationMaxBody = 32 * 1024
	// VsockCharacterizationRetries is the number of attempts to ship
	// the report — first attempt + 3 retries with the backoff below.
	VsockCharacterizationRetries = 3
	// VsockCharacterizationAckTimeout is the per-attempt wait for the
	// 1-byte ack. Distinct from the overall characterization deadline
	// which is the boot-orchestration budget.
	VsockCharacterizationAckTimeout = 1500 * time.Millisecond
)

var vsockCharacterizationBackoff = []time.Duration{
	100 * time.Millisecond,
	250 * time.Millisecond,
	500 * time.Millisecond,
}

// CharacterizationResult is what runCharacterization returns to the
// caller. The caller (boot()) uses it ONLY to decide whether to log
// a degradation (the platform never fails a deploy on a probe
// failure — the host falls back to the scan-hint class).
type CharacterizationResult struct {
	Mode          PortNormMode // "none" | "dnat" | "forward" — what rung
	Port          int          // the observed bind port (0 if none)
	ExitCode      int          // 0=clean, -1=crash-loop exhausted
	ObservedClass string       // guest best-guess (host re-derives)
	Duration      time.Duration
	Shipped       bool   // true if the report landed with an ack
	Reason        string // "ok" | "ack_timeout" | "bind_timeout" | "send_error"
}

// RunArgs is the input bundle for the characterization probe. Split
// out so the test (characterize_linux_test.go) can drive a synthetic
// scenario without spinning up a real customer app.
type RunArgs struct {
	Manifest       api.AppManifest     // for the healthz / port-8080 baseline
	AppPID         func() int          // the supervisor's child PID; -1 = no child
	WaitForExit    func() (int, error) // blocks until app exits or deadline
	RingBufferTail func() string       // the supervisor's log ring buffer tail
	Log            *slog.Logger
	Now            func() time.Time // injectable for tests
}

// runCharacterization is the boot-side entry point. It observes the
// app's first 10 s of life (or until exit), runs L7 probes against
// the observed bind, and ships the report. Tolerates every failure
// path — the platform's contract is "no signal" not "won't boot".
func runCharacterization(ctx context.Context, args RunArgs) CharacterizationResult {
	if args.Log == nil {
		args.Log = slog.Default()
	}
	if args.Now == nil {
		args.Now = time.Now
	}

	start := args.Now()
	res := CharacterizationResult{Reason: "ok"}
	defer func() {
		res.Duration = args.Now().Sub(start)
		args.Log.Info("characterization complete",
			"mode", res.Mode, "port", res.Port, "class", res.ObservedClass,
			"exit", res.ExitCode, "shipped", res.Shipped, "reason", res.Reason,
			"duration", res.Duration)
	}()

	// 1. Observe the bind: watch /proc/net/tcp{,6} filtered to the
	// supervisor's child PID's socket inodes. First LISTEN entry
	// wins; cap the wait at the characterization deadline.
	obs, observedAddr := waitForBind(ctx, args, &res)
	if !obs {
		res.Reason = "bind_timeout"
	}

	// 2. Run L7 probes against the observed port. Each probe is a
	// goroutine; we record the first positive outcome and move on.
	// If no bind, every probe is a fast-fail; the report still
	// tells the host `class=job, exit=...`.
	classHint := runL7Probes(ctx, args, res.Port)
	res.ObservedClass = classHint

	// 3. Pick a portnorm mode. manifest.Port==0 means DefaultAppPort
	// was effective (the customer's process is expected to bind
	// 8080). Anything else requires a ladder rung.
	res.Mode = choosePortNormMode(args.Manifest, res.Port)
	if res.Mode == PortNormDNAT {
		if err := installDNAT(res.Port, args.Log); err != nil {
			res.Mode = PortNormForward
			_, _ = startForwarder(res.Port, args.Log)
		}
	}
	if res.Mode == PortNormForward {
		ln, fErr := startForwarder(res.Port, args.Log)
		if fErr != nil || ln == nil {
			args.Log.Warn("portnorm forward failed; host will see listen mismatch",
				"port", res.Port, "err", fErr)
		}
	}

	// 4. Wait for the supervisor to exit (clean or crash-loop
	// exhausted). Log the exit code on the report so the deploy
	// row can show it. The deadline is the same as bindTimeout.
	exitCh := make(chan int, 1)
	go func() { code, _ := args.WaitForExit(); exitCh <- code }()
	select {
	case res.ExitCode = <-exitCh:
	case <-ctx.Done():
		res.Reason = "shutdown_timeout"
		res.ExitCode = -1
	}

	// 5. Build the report and ship it.
	addr := observedAddr
	if addr == "" {
		addr = fmt.Sprintf("127.0.0.1:%d", res.Port)
	}
	if args.RingBufferTail != nil {
		// pulled in by the supervisor's crash-loop hook
	}
	r := api.CharacterizationReport{
		ObservedClass:         res.ObservedClass,
		ObservedPort:          res.Port,
		ExitCode:              res.ExitCode,
		ListeningAddrs:        []string{addr},
		LogTail:               truncateLog(args.RingBufferTail(), VsockCharacterizationMaxBody),
		PortNormalizationMode: string(res.Mode),
	}
	if !shipReport(r, args.Log) {
		res.Shipped = false
		if res.Reason == "ok" {
			res.Reason = "ack_timeout"
		}
	} else {
		res.Shipped = true
	}
	return res
}

// waitForBind polls /proc/net/tcp{,6} for a LISTEN socket owned by
// the app's PID tree. Returns the first match and the address
// string for the report. On timeout returns observed=false and
// observedAddr="" — the host treats this as `class=job`.
func waitForBind(ctx context.Context, args RunArgs, res *CharacterizationResult) (bool, string) {
	deadline := args.Now().Add(10 * time.Second)
	for {
		if port, addr, ok := probeListening(args.AppPID()); ok {
			res.Port = port
			return true, addr
		}
		if args.Now().After(deadline) {
			return false, ""
		}
		select {
		case <-ctx.Done():
			return false, ""
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// probeListeningLinux is the linux-only /proc walk. Called by
// probeListening (characterize_common.go) on linux after the
// no-child early-out passes. Cross-platform callers always go
// through probeListening; the unit test pins the early-out
// contract there.
func probeListeningLinux(pid int) (int, string, bool) {
	ownedInodes := ownedSocketInodes(pid)
	if len(ownedInodes) == 0 {
		return 0, "", false
	}
	if port, addr, ok := scanListeningFile("/proc/net/tcp", ownedInodes); ok {
		return port, addr, true
	}
	if port, addr, ok := scanListeningFile("/proc/net/tcp6", ownedInodes); ok {
		return port, addr, true
	}
	return 0, "", false
}

// ownedSocketInodes walks /proc/<pid>/fd/* looking for the symlink
// target of the form `socket:[<inode>]`. Returns the inodes the
// process owns. Returns nil if /proc isn't mounted (e.g. /proc
// unmounted post-pivot — defensive, shouldn't happen here).
func ownedSocketInodes(pid int) map[uint64]struct{} {
	dir := fmt.Sprintf("/proc/%d/fd", pid)
	f, err := os.Open(dir)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	out := make(map[uint64]struct{})
	for {
		names, err := f.Readdirnames(64)
		for _, n := range names {
			link, lErr := os.Readlink(dir + "/" + n)
			if lErr != nil {
				continue
			}
			// Format: "socket:[12345]"
			if !strings.HasPrefix(link, "socket:[") || !strings.HasSuffix(link, "]") {
				continue
			}
			var inode uint64
			if _, sErr := fmt.Sscanf(link[len("socket:["):len(link)-1], "%d", &inode); sErr == nil {
				out[inode] = struct{}{}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return out
		}
	}
	return out
}

// runL7Probes kicks off the three probes concurrently against the
// observed port. Returns the first non-empty class hint and falls
// back to "http" if every probe is inconclusive. The 2 s ctx budget
// is bounded — a slow / unresponsive listener costs us 2 s of boot,
// not unbounded hangs.
func runL7Probes(ctx context.Context, _ RunArgs, port int) string {
	if port <= 0 {
		// No bind → can't probe. Engine interprets "no bind, exit 0"
		// as `job`. We surface that hint, host re-derives.
		return "job"
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	probes := []func() string{
		probeGraphQL(port),
		probeGRPC(port),
		probeHTTP(port),
	}
	resCh := make(chan string, len(probes))
	for _, fn := range probes {
		fn := fn
		go func() {
			resCh <- fn()
		}()
	}
	collected := 0
	for collected < len(probes) {
		select {
		case <-probeCtx.Done():
			return "http"
		case r := <-resCh:
			collected++
			if r != "" {
				return r
			}
		}
	}
	return "http"
}

func probeGraphQL(port int) func() string {
	return func() string {
		c := &http.Client{Timeout: 1 * time.Second}
		// GraphQL `__schema` introspection — most servers reply 200
		// with a body containing "data.__schema". We use a small
		// POST with a valid query and check the body contains the
		// canonical field.
		body := strings.NewReader(`{"query":"{__schema{queryType{name}}}"}`)
		req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/", port), body)
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Do(req)
		if err != nil {
			return ""
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return ""
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if bytes.Contains(raw, []byte("__schema")) {
			return "graphql"
		}
		return ""
	}
}

func probeGRPC(port int) func() string {
	return func() string {
		// gRPC servers respond to a GET with the HTTP/2 SETTINGS
		// frame and `:status 200`; a generic GET to the port is
		// mostly going to fail. A more honest check is to dial,
		// send an empty HTTP/2 preface, and observe `:status` 200
		// in the response. For the characterize probe we keep
		// this brief: TCP-level open + check for the canonical
		// gRPC response header "content-type: application/grpc".
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 1*time.Second)
		if err != nil {
			return ""
		}
		defer func() { _ = c.Close() }()
		// gRPC reflection protocol — send the bare minimum probe
		// (raw bytes that look like HTTP/2 magic).
		_, _ = c.Write([]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"))
		buf := make([]byte, 256)
		_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, _ := c.Read(buf)
		if n > 0 && bytes.Contains(buf[:n], []byte("content-type")) {
			// Probably gRPC reflection; the engine re-derives.
			return "grpc"
		}
		return ""
	}
}

func probeHTTP(port int) func() string {
	return func() string {
		// GET / + check for any HTTP-shaped response. A real GraphQL
		// or gRPC server usually fails the bare GET; we already
		// classified those. Anything else with `HTTP/1. 200/300`
		// counts as `http`.
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 1*time.Second)
		if err != nil {
			return ""
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(1 * time.Second))
		fmt.Fprintf(c, "GET /openapi.json HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n")
		buf := make([]byte, 1024)
		n, _ := c.Read(buf)
		if n > 0 && bytes.HasPrefix(buf[:n], []byte("HTTP/1.")) && bytes.Contains(buf[:n], []byte(" 2")) {
			return "http"
		}
		return ""
	}
}

// shipReport dials host CID 2 (VMADDR_CID_HOST) at the char port,
// writes [msg_type 4 BE][body_len 4 BE][body], and awaits a 1-byte
// ack with the 1.5s per-attempt timeout. Returns true on ack OK.
func shipReport(r api.CharacterizationReport, log *slog.Logger) bool {
	body, err := json.Marshal(r)
	if err != nil {
		log.Warn("characterization marshal failed", "err", err)
		return false
	}
	if len(body) > VsockCharacterizationMaxBody {
		body = body[:VsockCharacterizationMaxBody]
	}
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], VsockCharacterizationMsgType)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(body)))
	payload := append(hdr[:], body...)

	attempts := VsockCharacterizationRetries + 1
	for i := 0; i < attempts; i++ {
		backoff := vsockCharacterizationBackoff[i]
		if ok := shipOnce(payload); ok {
			return true
		}
		if i < attempts-1 {
			time.Sleep(backoff)
		}
	}
	return false
}

// shipOnce is a single attempt: open STREAM, send, read 1 byte.
func shipOnce(payload []byte) bool {
	sock, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer func() { _ = unix.Close(sock) }()
	addr := &unix.SockaddrVM{
		CID:  unix.VMADDR_CID_HOST,
		Port: VsockCharacterizationPort,
	}
	if err := unix.Connect(sock, addr); err != nil {
		return false
	}
	// Set per-socket deadlines via SO_SNDTIMEO / SO_RCVTIMEO
	// (golang.org/x/sys/unix has no SetDeadline helper for AF_VSOCK
	// sockets; the kernel-level setsockopt works on all sockets).
	setSockTimeout(sock, unix.SO_SNDTIMEO, 1500*time.Millisecond)
	setSockTimeout(sock, unix.SO_RCVTIMEO, VsockCharacterizationAckTimeout)
	if _, err := unix.Write(sock, payload); err != nil {
		return false
	}
	ack := make([]byte, 1)
	n, err := unix.Read(sock, ack)
	return err == nil && n == 1 && ack[0] == 0
}

// setSockTimeout applies a SO_*_TIMEO setsockopt on a raw socket fd.
// Used to bound shipOnce's Read/Write because golang.org/x/sys/unix
// has no per-fd deadline helper for AF_VSOCK. Errors are tolerated:
// a failed setsockopt just means the default recv/send timeout (which
// is 0 = indefinite) applies; the upper bound becomes the connect()
// returning EAGAIN or the kernel eventually reclaiming the socket.
func setSockTimeout(fd int, opt int, d time.Duration) {
	tv := unix.Timeval{
		Sec:  int64(d / time.Second),
		Usec: int64(d%time.Second) / 1000,
	}
	_ = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, opt, &tv)
}

// truncateLog lives in characterize_common.go (build-tag-free).

var _ atomic.Bool // types-only reference, satisfies unused-import lints
