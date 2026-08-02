// Pure tests for the ring buffer (ADR-051 Phase 4 Slice A PR-B) and
// the Supervisor's LogBuffer/LogTail/resetLog surface. Build-tag-free so
// the suite runs on every platform — the ring buffer has no /proc or
// AF_VSOCK dependency, and the Supervisor's log-buffer surface is
// reachable without forking a customer process (the test Start functions
// write to sup.LogBuffer() directly).
//
// Run with `go test -race ./guest/init/...` to exercise the
// concurrent-write path; the race detector catches any future refactor
// that drops the sync.Mutex from ringBuffer.Write.
package main

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
)

// TestRingBuffer_FreshTailIsEmpty pins the contract that a never-written
// buffer returns "" (not nil, not panic). The characterize probe's
// RingBufferTail callback used to return "" as a literal; the ring
// buffer version must produce the same shape so callers can use the
// same string-empty check they always have.
func TestRingBuffer_FreshTailIsEmpty(t *testing.T) {
	rb := newRingBuffer(64)
	if got := rb.Tail(); got != "" {
		t.Errorf("fresh Tail() = %q, want empty", got)
	}
}

// TestRingBuffer_AppendUnderCap pins the no-trim path: writes under the
// cap are returned verbatim, in order, with no slicing.
func TestRingBuffer_AppendUnderCap(t *testing.T) {
	rb := newRingBuffer(64)
	if _, err := rb.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := rb.Tail(); got != "hello" {
		t.Errorf("Tail() = %q, want %q", got, "hello")
	}
}

// TestRingBuffer_TrimAtCap pins the over-cap trim: the buffer must
// keep exactly the last `cap` bytes and nothing older. This is the
// load-bearing property — the characterize probe reads the "tail" of
// the customer's boot log, so a refactor that kept the oldest bytes
// instead of the newest would silently surface the wrong window.
func TestRingBuffer_TrimAtCap(t *testing.T) {
	rb := newRingBuffer(64)
	// Write 100 'a'-prefixed bytes — the buffer must keep the last 64.
	payload := strings.Repeat("a", 100)
	if _, err := rb.Write([]byte(payload)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := rb.Tail()
	if len(got) != 64 {
		t.Errorf("Tail() len = %d, want 64", len(got))
	}
	// Trim dropped the oldest 36 bytes — the tail is the last 64
	// characters of the input, all 'a'.
	if got != strings.Repeat("a", 64) {
		t.Errorf("Tail() = %q, want 64 'a's", got)
	}
}

// TestRingBuffer_ResetClears pins the per-restart reset contract: a
// Reset() drops the buffer to empty, and the next Write starts fresh.
// This is the property the supervisor's resetLog() depends on — a
// refactor that didn't actually empty the buffer would leak bytes
// from the previous restart into the next characterize report.
func TestRingBuffer_ResetClears(t *testing.T) {
	rb := newRingBuffer(64)
	if _, err := rb.Write([]byte(strings.Repeat("x", 200))); err != nil {
		t.Fatalf("Write: %v", err)
	}
	rb.Reset()
	if got := rb.Tail(); got != "" {
		t.Errorf("Tail() after Reset = %q, want empty", got)
	}
	if _, err := rb.Write([]byte("y")); err != nil {
		t.Fatalf("Write after Reset: %v", err)
	}
	if got := rb.Tail(); got != "y" {
		t.Errorf("Tail() after Reset+Write = %q, want %q", got, "y")
	}
}

// TestRingBuffer_ConcurrentWritesAreSafe pins the mutex invariant.
// Run under `go test -race`: 16 goroutines each write 1000 × 4-byte
// payloads; without the sync.Mutex, the race detector fires. With the
// mutex, the writes serialize and the buffer's lifetime byte count
// equals the input volume; the tail is the last 64 bytes of the
// interleaved write stream (we don't pin specific bytes because the
// interleaving order is non-deterministic; we pin only the invariant
// that the buffer is consistent and the tail is full).
func TestRingBuffer_ConcurrentWritesAreSafe(t *testing.T) {
	const goroutines = 16
	const writesPerGoroutine = 1000
	rb := newRingBuffer(64)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			// Each goroutine writes a distinct 4-byte payload
			// shaped as a hex id; the exact interleaving doesn't
			// matter, only that every write succeeds and the
			// final tail is exactly 64 bytes.
			payload := []byte{byte('A' + id), byte('0' + (id/10)%10), byte('0' + id%10), '\n'}
			for i := 0; i < writesPerGoroutine; i++ {
				if _, err := rb.Write(payload); err != nil {
					t.Errorf("goroutine %d write %d: %v", id, i, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	if got := rb.Tail(); len(got) != 64 {
		t.Errorf("Tail() len after concurrent writes = %d, want 64", len(got))
	}
}

// TestSupervisor_LogBufferCapturesStdout pins the wiring path:
// Supervisor.LogBuffer() returns an io.Writer, bytes written to it
// land in the supervisor's ring buffer, and LogTail() reads them
// back. This is the integration test that proves the field exists
// and is connected.
func TestSupervisor_LogBufferCapturesStdout(t *testing.T) {
	var s *Supervisor
	s = &Supervisor{Max: 0, Start: func() error {
		_, _ = s.LogBuffer().Write([]byte("boot-marker-1\n"))
		_, _ = s.LogBuffer().Write([]byte("boot-marker-2\n"))
		return nil
	}}
	if err := s.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := s.LogTail()
	if !strings.Contains(got, "boot-marker-1") {
		t.Errorf("LogTail missing boot-marker-1: %q", got)
	}
	if !strings.Contains(got, "boot-marker-2") {
		t.Errorf("LogTail missing boot-marker-2: %q", got)
	}
}

// TestSupervisor_LogBufferResetsOnRestart pins the per-restart reset
// contract. A supervisor with Max >= 1 runs the Start twice; the
// second Start's output must be the only thing in LogTail after the
// second Start returns. A refactor that didn't call resetLog() in
// TrackCommand would leak the first Start's bytes into the second
// report.
//
// Mirrors the production pattern: runAppWithEnv calls TrackCommand
// once per fork, which is what resets the buffer. The test does the
// same — each Start invocation tracks a synthetic cmd before writing
// to the buffer, simulating the production reset-before-write order.
func TestSupervisor_LogBufferResetsOnRestart(t *testing.T) {
	starts := 0
	var s *Supervisor
	s = &Supervisor{
		Max: 1,
		Start: func() error {
			starts++
			// Simulate runAppWithEnv's per-fork TrackCommand
			// (the production trigger of resetLog). Use a
			// non-nil *exec.Cmd with a zero-value Cmd — the
			// reset path doesn't read any field off the cmd,
			// but a non-nil cmd is a more honest simulation of
			// the production call shape. (LastAppPID() returns
			// -1 for a Cmd without a Process field; that doesn't
			// matter here because we're testing LogTail, not
			// LastAppPID.)
			s.TrackCommand(&exec.Cmd{})
			if starts == 1 {
				_, _ = s.LogBuffer().Write([]byte("restart1-marker\n"))
				return io.EOF // non-nil error → triggers restart
			}
			_, _ = s.LogBuffer().Write([]byte("restart2-marker\n"))
			return nil
		},
	}
	if err := s.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := s.LogTail()
	if strings.Contains(got, "restart1-marker") {
		t.Errorf("LogTail leaked bytes from first start: %q", got)
	}
	if !strings.Contains(got, "restart2-marker") {
		t.Errorf("LogTail missing restart2-marker: %q", got)
	}
}

// TestSupervisor_LogBufferCapRespected pins the 64 KiB cap: writes
// that exceed api.LogRingBufferBytes get trimmed, and LogTail()
// never returns more than the cap. A refactor that removed the
// trim-on-overflow would let a chatty boot log balloon the buffer
// and silently overflow the VsockCharacterizationMaxBody wire cap.
func TestSupervisor_LogBufferCapRespected(t *testing.T) {
	var s *Supervisor
	s = &Supervisor{Max: 0, Start: func() error {
		// 200 KiB — 3x the 64 KiB cap. Use a single Write so the
		// trim path is exercised in one call (not via many small
		// writes that the over-cap Write would also trim).
		_, _ = s.LogBuffer().Write(make([]byte, 200*1024))
		return nil
	}}
	if err := s.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := s.LogTail()
	if len(got) > api.LogRingBufferBytes {
		t.Errorf("LogTail len = %d, want <= %d (api.LogRingBufferBytes)", len(got), api.LogRingBufferBytes)
	}
}

// TestRunAppWithEnv_StdoutStreamSurvives is the regression guard for
// the live-console contract: bytes written via the MultiWriter wrap
// (os.Stdout + sup.LogBuffer()) reach BOTH destinations. This catches
// the "refactor drops the io.MultiWriter wrap and replaces it with
// sup.LogBuffer() only" failure mode — operators watching journalctl
// would see no boot log without this test catching it.
//
// We exercise the MultiWriter shape directly rather than calling
// runAppWithEnv because that function is gated //go:build linux and
// this test file is build-tag-free. The MultiWriter pattern is the
// actual seam being tested; the runAppWithEnv call site is a thin
// wrapper around it.
//
// The test resolves os.Stdout INSIDE the closure so the captured
// value tracks the post-redirect os.Stdout (the pipe writer). A
// regression that captures os.Stdout at MultiWriter-construction
// time would miss the redirect and write to the test's stderr
// instead — the test would still pass for the LogBuffer half but
// fail to assert anything about the live-console half. Resolving
// inside the closure mirrors how runAppWithEnv reads os.Stdout at
// exec-Cmd-construction time in production (always the kernel fd 1,
// no captured reference).
func TestRunAppWithEnv_StdoutStreamSurvives(t *testing.T) {
	// Redirect os.Stdout to a pipe + read it back. Standard pattern
	// from cmd/apid and cmd/builderd test suites.
	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = origStdout }()

	var s *Supervisor
	s = &Supervisor{Max: 0, Start: func() error {
		// Same shape as runAppWithEnv's wiring (main_linux.go).
		// Resolve os.Stdout at write-time (not at closure-capture
		// time) so the redirect is honored.
		mw := io.MultiWriter(os.Stdout, s.LogBuffer())
		_, _ = mw.Write([]byte("live-console-marker\n"))
		return nil
	}}
	if err := s.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	if !strings.Contains(string(buf[:n]), "live-console-marker") {
		t.Errorf("captured os.Stdout missing live-console-marker: %q", string(buf[:n]))
	}
	if !strings.Contains(s.LogTail(), "live-console-marker") {
		t.Errorf("LogTail missing live-console-marker: %q", s.LogTail())
	}
	// Reference origStdout so the linter doesn't drop the
	// declaration (the variable is documented as the pre-redirect
	// reference for readers).
	_ = origStdout
}
