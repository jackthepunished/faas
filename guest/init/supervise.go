package main

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync/atomic"

	"github.com/onebox-faas/faas/pkg/api"
)

// Supervisor runs the customer app and restarts it on crash up to Max times,
// then gives up so the VM exits (spec §4.8). The process start is injected so the
// restart policy is unit-tested without spawning anything.
//
// ADR-051 Phase 4: the characterize probe (characterize_linux.go) needs to
// observe the running app's PID + captured exit code from OUTSIDE the
// supervisor loop. We expose that state via the LastCmd / LastExitCode
// atomic pair — the characterize probe reads them under a short spin, and
// the boot() glue waits on Run()'s return the same way it always has.
type Supervisor struct {
	Max     int                          // max restarts after the initial start
	Start   func() error                 // runs the app to completion; nil = clean exit
	OnCrash func(attempt int, err error) // optional hook for logging/backoff

	// LastCmd tracks the *exec.Cmd the supervisor most-recently forked,
	// swapped atomically on every restart. nil until the first fork.
	// Read by characterize_linux.go's AppPID() callback.
	lastCmd atomic.Pointer[exec.Cmd]
	// lastExitCode is the terminal exit code observed by the most-
	// recent successful Start. -1 until the first observation.
	lastExitCode atomic.Int64
	// lastLog is the supervisor's stdout/stderr ring buffer, allocated
	// lazily on the first LogBuffer() call. Reset on every restart by
	// TrackCommand (see TrackCommand's doc comment for the reset-
	// before-store ordering rationale). Read by characterize_linux.go's
	// RingBufferTail() callback via LogTail() to populate the report's
	// LogTail field for the deploy row.
	//
	// atomic.Pointer matches lastCmd above — Supervisor stays
	// non-copyable per the copylocks/atomic noCopy invariant. The
	// lazy-allocation pattern means a Supervisor that never has its
	// log buffer read pays zero allocation cost (matters for tests
	// that exercise the supervisor with no characterize probe).
	lastLog atomic.Pointer[ringBuffer]
	// lastRunErr (issue #463 / ADR-069 / PR-B) is the terminal
	// error from the most-recent Run() invocation. The orchestrator
	// (runWorkloads) reads this after WaitGroup.Wait() to surface
	// the main workload's terminal state. nil = clean exit or
	// never ran.
	lastRunErr atomic.Pointer[error]
}

// LastExitCode returns -1 if no fork has observed an exit yet;
// otherwise the exit code of the most-recently-completed Start.
// Thread-safe.
func (s *Supervisor) LastExitCode() int {
	return int(s.lastExitCode.Load())
}

// TrackCommand records the *exec.Cmd the supervisor most-recently
// forked, swapped atomically on every restart. Read by
// characterize_linux.go's AppPID() callback via LastAppPID.
// Invariant: callers call TrackCommand exactly once per fork
// (runAppWithEnv at guest/init/main_linux.go) — the same scope
// that calls cmd.Run(). A forked-but-not-tracked cmd means
// LastAppPID returns -1 and the characterize probe's bind-wait
// times out (correctly classified as `job`).
//
// Also resets the log ring buffer (if allocated) so the new fork
// starts a fresh 64 KiB window. Reset-before-store ordering: a
// concurrent LastAppPID+LogTail reader that sees the new PID will
// also see the empty buffer, never the previous restart's bytes
// paired with the new PID. The characterize probe spins anyway, so
// either ordering is "correct"; reset-before-store is the slightly
// safer pairing for operators reading the LogTail audit.
func (s *Supervisor) TrackCommand(cmd *exec.Cmd) {
	s.resetLog() // fresh window per restart (Slice A PR-B contract)
	s.lastCmd.Store(cmd)
}

// LastAppPID returns the PID of the most-recently-forked customer
// app, or -1 if the supervisor hasn't forked yet.
func (s *Supervisor) LastAppPID() int {
	if cmd := s.lastCmd.Load(); cmd != nil && cmd.Process != nil {
		return cmd.Process.Pid
	}
	return -1
}

// LogBuffer returns an io.Writer that captures bytes written to it
// into the supervisor's ring buffer. The buffer is allocated on the
// first call (lazy); subsequent calls return the same buffer so the
// customer's stdout/stderr pipes share a single capture window per
// fork. The returned writer is goroutine-safe (the underlying
// ringBuffer holds a sync.Mutex).
//
// runAppWithEnv (guest/init/main_linux.go) wraps this writer in an
// io.MultiWriter(os.Stdout, ...) so live console output survives —
// operators watching journalctl -u faas-vmmd still see the boot log
// streaming. The ring buffer captures a copy for the characterize
// report's LogTail field.
func (s *Supervisor) LogBuffer() io.Writer {
	// Fast path: buffer already allocated.
	if rb := s.lastLog.Load(); rb != nil {
		return rb
	}
	// Slow path: allocate and CAS-store. The loop is wait-free for
	// the common case where no concurrent caller raced us; in the
	// contended case (two goroutines call LogBuffer() concurrently
	// for the first time), exactly one wins the CAS and the other
	// loads the winner's buffer on retry.
	rb := newRingBuffer(api.LogRingBufferBytes)
	if s.lastLog.CompareAndSwap(nil, rb) {
		return rb
	}
	return s.lastLog.Load()
}

// LogTail returns the most-recent captured bytes (up to
// api.LogRingBufferBytes). Returns "" if the buffer has not been
// allocated yet (no LogBuffer() call has happened) — same shape as
// the empty-string sentinel the RingBufferTail callback returned
// before PR-B. Read by characterize_linux.go's RingBufferTail
// callback to populate the report's LogTail field.
func (s *Supervisor) LogTail() string {
	if rb := s.lastLog.Load(); rb != nil {
		return rb.Tail()
	}
	return ""
}

// resetLog clears the ring buffer (if allocated). Called from
// TrackCommand on every fork so each restart's capture window is
// independent. Safe to call when the buffer has never been allocated
// (a no-op); this lets TrackCommand call resetLog unconditionally
// without paying the cost of an allocation check.
func (s *Supervisor) resetLog() {
	if rb := s.lastLog.Load(); rb != nil {
		rb.Reset()
	}
}

// trackExit is called when Start returns; -1 indicates non-ExitError.
func (s *Supervisor) trackExit(code int) {
	s.lastExitCode.Store(int64(code))
}

// lastErr (issue #463 / ADR-069 / PR-B) returns the terminal
// error from the most-recent Run() invocation, or nil if the
// supervisor never ran. The orchestrator (runWorkloads) reads
// this after WaitGroup.Wait() returns to surface the main
// workload's terminal state. Sidecar supervisors' lastErr() is
// logged but ignored — non-essential sidecars can exit 0 and
// the deploy still succeeds.
//
// nil in two distinct cases: (1) supervisor never ran (the
// caller didn't dispatch Run on it), or (2) Run returned nil
// (clean exit). The orchestrator only consults lastErr() on
// supervisors it dispatched, so case (1) is impossible.
func (s *Supervisor) lastErr() error {
	if p := s.lastRunErr.Load(); p != nil {
		return *p
	}
	return nil
}

// trackRunErr records the terminal error from Run(). Called
// inside Run() itself before it returns so the orchestrator
// can read it after WaitGroup.Wait(). atomic.Pointer matches
// the other state fields — Supervisor stays non-copyable.
func (s *Supervisor) trackRunErr(err error) {
	s.lastRunErr.Store(&err)
}

// Run starts the app and supervises it. It returns nil if the app ever exits
// cleanly, or the last error once the restart budget is exhausted.
//
// ADR-051 Phase 4: pointer receiver — embedding atomic.Pointer / atomic.Int64
// in the struct makes Supervisor non-copyable (`go vet`'s `copylocks` +
// `atomic`'s `noCopy` are both load-bearing here). Earlier signature was
// value-receiver; this change is the canonical fix.
func (s *Supervisor) Run() error {
	restarts := 0
	for {
		err := s.Start()
		// Record the most-recent exit (or -1) so characterize can
		// surface it on the report. Mirrors what Run sees.
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				s.trackExit(ee.ExitCode())
			} else {
				s.trackExit(-1)
			}
		} else {
			s.trackExit(0)
		}
		if err == nil {
			s.trackRunErr(nil)
			return nil // clean exit; nothing to supervise
		}
		if restarts >= s.Max {
			final := fmt.Errorf("app crash-looped after %d restart(s): %w", restarts, err)
			s.trackRunErr(final)
			return final
		}
		restarts++
		if s.OnCrash != nil {
			s.OnCrash(restarts, err)
		}
	}
}
