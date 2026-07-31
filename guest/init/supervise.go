package main

import (
	"errors"
	"fmt"
	"os/exec"
	"sync/atomic"
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
	// LastLogBytes is a 64 KiB ring buffer of the supervisor's
	// stdout+stderr capture. Reset on every restart. Read by
	// characterize_linux.go's RingBufferTail() callback to populate
	// the report's LogTail field for the deploy row.
}

// LastExitCode returns -1 if no fork has observed an exit yet;
// otherwise the exit code of the most-recently-completed Start.
// Thread-safe.
func (s *Supervisor) LastExitCode() int {
	return int(s.lastExitCode.Load())
}

// LastAppPID returns the PID of the most-recently-forked customer
// app, or -1 if the supervisor hasn't forked yet.
func (s *Supervisor) LastAppPID() int {
	if cmd := s.lastCmd.Load(); cmd != nil && cmd.Process != nil {
		return cmd.Process.Pid
	}
	return -1
}

// trackExit is called when Start returns; -1 indicates non-ExitError.
func (s *Supervisor) trackExit(code int) {
	s.lastExitCode.Store(int64(code))
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
			return nil // clean exit; nothing to supervise
		}
		if restarts >= s.Max {
			return fmt.Errorf("app crash-looped after %d restart(s): %w", restarts, err)
		}
		restarts++
		if s.OnCrash != nil {
			s.OnCrash(restarts, err)
		}
	}
}
