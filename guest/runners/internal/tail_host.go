// Package internal — shared runtime helpers for the guest runners
// (node22, node24, python312, python313, go124). The tail host
// helper here is the one place the runner shims call when the
// customer's first non-5xx response lands and we need to drain
// every registered waitUntil(promise) task before the response
// is read (issue #667 / ADR-078, the consolidated follow-up PR).
//
// Wire (runner → tail host proxy):
//
//	customer's waitUntil(promise) shim appends one JSON line per
//	registration to envelope.TailPipePath:
//
//	  {"id":"task-N","wait":false,"err":""}
//
//	the runner spawns a goroutine per line, runs the task under
//	context.WithTimeout(envelope.WaitUntilSec), and on terminal
//	(completed / failed / timeout) writes a line to the
//	/run/guest-init/tail-events.sock proxy:
//
//	  "<outcome_byte> <elapsed_ms>\n"
//
//	The proxy at /run/guest-init/tail-events.sock (see
//	guest/init/tail_events_proxy_linux.go) accepts the line,
//	frames the 16-byte vsock DGRAM body
//	[1B type=0x04][1B outcome][6B reserved][8B elapsed_ms BE uint64],
//	and forwards to vmmd on vsock port 1027.
//
// The runner side stays narrow: one line of text per terminal, no
// marshalling. Errors are NOT propagated to the HTTP response —
// they're appended to response.TailErrors (debug-only, surfaced
// via runner stderr + schedd audit rows). The runner keeps
// draining on a lost receipt; the 5s snapshotAndPark watchdog
// is the upper bound on lost tails.
package internal

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// TailEventsProxyPath is the unix-socket path the runners
// connect to. Duplicated from guest/init/tail_events_proxy_linux.go
// because guest/runners doesn't import guest/init (separate
// binaries compiled into different images). The constant MUST
// stay in sync with guest/init/tail_events_proxy_linux.go's
// TailEventsProxyPath.
const TailEventsProxyPath = "/run/guest-init/tail-events.sock"

// TailDialTimeout caps how long the runner waits on the proxy
// accept. The proxy is started in boot() before the runners fork
// so a healthy guest sees a near-zero connect time; the timeout
// is the "stale socket from a previous boot" / "proxy not yet
// up" safety net. Mirrors FrameworkReadyDialTimeout.
const TailDialTimeout = 250 * time.Millisecond

// TailWriteTimeout caps the write of one line to the proxy.
// 250ms is generous — the proxy reads one line and replies with
// "ok\n" or "err <reason>\n".
const TailWriteTimeout = 250 * time.Millisecond

// TailLine mirrors the JSON shape the customer's waitUntil(promise)
// shim writes to envelope.TailPipePath. The id field is informational
// (the runner echoes it into TailErrors for debug; the host doesn't
// read it). wait=true is reserved for future use (e.g. blocking wait
// until promise resolves); today every line is a fire-and-forget
// tail with no wait. err is set by the shim if the registration
// itself failed before the runner saw it.
//
// Exported so the per-runner drainTailHost shim can declare a
// matching closure receiver without re-deriving the JSON tags.
// The wire shape is the only contract — the runner's tail host
// matches by json.Unmarshal in ReadPipe (see below).
type TailLine struct {
	ID   string `json:"id"`
	Wait bool   `json:"wait"`
	Err  string `json:"err,omitempty"`
}

// TailOutcome byte constants — mirror pkg/fcvm.TailOutcomeCompleted,
// TailOutcomeFailed, TailOutcomeTimeout so the runner-side encoding
// matches the host-side decode in cmd/vmmd/framework_ready_recv.go.
// Keep this in sync with guest/init/sidecar_events_proxy_linux.go's
// tailEventOutcome* constants.
const (
	TailOutcomeCompleted byte = 1
	TailOutcomeFailed    byte = 2
	TailOutcomeTimeout   byte = 3
)

// TailHost owns the runner-side drain of envelope.TailPipePath.
// One TailHost per wake (one per process — the runner is the
// long-lived process in the VM). The drain starts BEFORE the
// response is written and blocks until every registered task
// reaches a terminal state. The 5s snapshotAndPark watchdog on
// the host side is the upper bound if the drain hangs.
type TailHost struct {
	runtime     string
	pipePath    string
	waitUntil   time.Duration
	tailCapMax  int

	mu         sync.Mutex
	registered int          // total tails registered (bounded by tailCapMax)
	failures   []string     // drained into resp.TailErrors on Wait()

	wg         sync.WaitGroup
}

// NewTailHost returns a fresh drain. runtime is the runner id
// ("node22", "python312", etc.); pipePath is envelope.TailPipePath
// (the JSONL pipe the customer shim writes to); waitUntilSec is
// envelope.WaitUntilSec (per-task wall-clock ceiling; 0 = drain
// disabled — caller should skip Drain entirely); tailCapMax is
// the structural ceiling on concurrent in-flight tails (the
// pkg/api/limits.go TailCapMax constant, pinned at 16 today).
func NewTailHost(runtime, pipePath string, waitUntilSec int, tailCapMax int) *TailHost {
	return &TailHost{
		runtime:    runtime,
		pipePath:   pipePath,
		waitUntil:  time.Duration(waitUntilSec) * time.Second,
		tailCapMax: tailCapMax,
		failures:   nil,
	}
}

// Failures returns the per-task failure list accumulated during
// Drain(). The runner marshals these into response.TailErrors
// after Wait() returns. nil = every task completed (or no tasks
// were registered).
func (h *TailHost) Failures() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.failures))
	copy(out, h.failures)
	return out
}

// RegisterCount returns the number of tails registered so far.
// Used by the runner to assert the customer honored TailCapMax
// before draining — a customer that registered 17 tasks against
// a 16-cap will see the 17th dropped (and a counter increment in
// pkg/wire/metrics.TailCapReached).
func (h *TailHost) RegisterCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.registered
}

// Register spawns one tail-task goroutine. Returns false if the
// cap has been reached (caller increments the cap-reached
// counter and drops the registration). Each registered task is
// bounded by the TailHost's waitUntil ceiling; on expiry the
// runner cancels the embedded context and emits a 0x04 timeout
// DGRAM.
//
// taskID is the customer's shim-assigned id (informational —
// echoed into TailErrors on timeout). taskFn is the goroutine
// body; the runner passes a closure that invokes the customer's
// promise. The closure receives a context that is cancelled at
// the waitUntil ceiling — the customer's promise must honor it.
func (h *TailHost) Register(taskID string, taskFn func(ctx context.Context)) bool {
	h.mu.Lock()
	if h.tailCapMax > 0 && h.registered >= h.tailCapMax {
		h.mu.Unlock()
		return false
	}
	h.registered++
	h.mu.Unlock()

	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		h.runTask(taskID, taskFn)
	}()
	return true
}

// Drain blocks until every registered task reaches a terminal
// state, or the waitUntil ceiling has passed since the LAST
// registration. The runner calls Drain() after the customer's
// handler subprocess returns successfully and BEFORE the
// response envelope is written — so the customer sees the tail
// drain window close before the wake signals framework_ready.
//
// A safety-net deadline caps the drain at waitUntil + 250ms
// (the same slack FrameworkReadyWriteTimeout grants the proxy),
// so a hung tail task can never block the runner for more than
// the ceiling + slack. The snapshotAndPark watchdog on the host
// side is the outer bound.
func (h *TailHost) Drain() {
	done := make(chan struct{})
	go func() {
		h.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return
	case <-time.After(h.waitUntil + TailWriteTimeout):
		// Hung drain — the outer bound is the snapshotAndPark
		// 5s watchdog on the host. We log to stderr so the
		// runner's existing log scrape surfaces the hang.
		fmt.Fprintf(os.Stderr, "tail_host: drain timeout after %s; %d tails may be lost\n",
			h.waitUntil+TailWriteTimeout, h.RegisterCount())
	}
}

// runTask executes one tail-task goroutine. On terminal state
// it appends a failure entry to h.failures (only on err/timeout)
// and emits a 0x04 DGRAM via the tail-events proxy. Errors from
// the proxy are logged at Warn; the runner keeps draining.
func (h *TailHost) runTask(taskID string, taskFn func(ctx context.Context)) {
	ctx, cancel := context.WithTimeout(context.Background(), h.waitUntil)
	defer cancel()

	start := time.Now()
	panicked := true
	outcome := TailOutcomeFailed
	defer func() {
		elapsedMs := time.Since(start).Milliseconds()
		if panicked {
			outcome = TailOutcomeFailed
			h.recordFailure(fmt.Sprintf("error:%s", taskID))
		}
		if err := h.emit(outcome, elapsedMs); err != nil {
			fmt.Fprintf(os.Stderr, "tail_host: emit 0x%02x for %s failed: %v\n", outcome, taskID, err)
		}
	}()

	taskFn(ctx)

	// If ctx.Err() == DeadlineExceeded, the per-task ceiling fired.
	// We can't tell from inside the goroutine whether taskFn honored
	// the cancellation — but if it's still running, the deferred
	// cancel will let it return on its own (the customer's promise
	// is expected to check ctx.Done()). We mark the outcome based
	// on whether the deadline elapsed.
	if ctx.Err() == context.DeadlineExceeded {
		outcome = TailOutcomeTimeout
		h.recordFailure(fmt.Sprintf("timeout:%s", taskID))
		return
	}
	outcome = TailOutcomeCompleted
	panicked = false
}

// recordFailure appends one entry to h.failures under the mu.
func (h *TailHost) recordFailure(reason string) {
	h.mu.Lock()
	h.failures = append(h.failures, reason)
	h.mu.Unlock()
}

// emit writes one line to the tail-events proxy, framing the
// 16-byte DGRAM on the guest-init side. Mirrors
// framework_ready.go::signalFrameworkReady's dial/write shape
// exactly (same dial deadline, same write deadline, same
// "ok\n" / "err <reason>\n" reply shape).
func (h *TailHost) emit(outcome byte, elapsedMs int64) error {
	d := net.Dialer{Timeout: TailDialTimeout}
	conn, err := d.Dial("unix", TailEventsProxyPath)
	if err != nil {
		return fmt.Errorf("dial tail-events proxy: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.SetWriteDeadline(time.Now().Add(TailWriteTimeout)); err != nil {
		return fmt.Errorf("set write deadline: %w", err)
	}
	line := fmt.Sprintf("%d %d\n", outcome, elapsedMs)
	if _, err := conn.Write([]byte(line)); err != nil {
		return fmt.Errorf("write line: %w", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(TailWriteTimeout)); err != nil {
		return fmt.Errorf("set read deadline: %w", err)
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		return fmt.Errorf("read reply: %w", err)
	}
	reply := string(buf[:n])
	if reply != "ok\n" {
		return fmt.Errorf("proxy rejected: %s", reply)
	}
	return nil
}

// ReadPipe reads envelope.TailPipePath line-by-line and calls
// onLine for each non-empty line. The runner invokes this BEFORE
// Drain() to consume any lines the customer wrote during the
// handler's invocation window — the file is unlinked at the
// runner's boot (or stays around from a previous boot; in either
// case, the runner reads from offset 0 and discards on close).
//
// Errors reading the pipe are logged at Warn — a missing pipe
// means the customer never called waitUntil, which is the
// expected 99% case. The runner keeps draining.
func ReadPipe(pipePath string, onLine func(line TailLine)) error {
	if pipePath == "" {
		return nil
	}
	f, err := os.Open(pipePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open pipe: %w", err)
	}
	defer func() { _ = f.Close() }()

	r := bufio.NewReader(f)
	for {
		raw, err := r.ReadBytes('\n')
		if len(raw) > 0 {
			var line TailLine
			if jerr := json.Unmarshal(raw[:len(raw)-1], &line); jerr == nil && line.ID != "" {
				onLine(line)
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read pipe: %w", err)
		}
	}
}
