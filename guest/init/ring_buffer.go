// Pure byte ring for the Supervisor's stdout/stderr capture (ADR-051
// Phase 4 Slice A PR-B). Build-tag-free so the test suite exercises it
// on every platform — the buffer has no /proc or AF_VSOCK dependency.
//
// The buffer is single-purpose: capture the customer app's stdout/stderr
// during a single Start cycle, hand the last N bytes to the characterize
// probe. Reused across restarts (Reset() drops the slice to length 0 and
// reuses the underlying capacity). Memory is bounded by `cap` regardless
// of how many bytes flow through it.
package main

import (
	"sync"
)

// ringBuffer is a fixed-capacity tail-only byte ring. The implementation
// keeps a single growing slice `buf` and trims by re-slicing from the back
// when the slice exceeds `cap`. The head/tail index pair approach (used
// in some ring buffer implementations) trades a simpler Write for a more
// complex Read — we want the simpler Read because the characterize probe
// is the hot path and the supervisor's exec'd process pipes are not.
//
// Capacity is pinned at construction; re-sizing is not supported and not
// needed. A single allocation per Supervisor (one per guest) is the
// memory budget.
type ringBuffer struct {
	mu  sync.Mutex
	buf []byte
	cap int
	// n is the running total of bytes ever written. Tracked so Reset()
	// can confirm a "fresh" vs "wrapped" state without inspecting `buf`,
	// and so a future debug log can surface the lifetime byte count
	// without re-walking the slice. Not used by Tail() — Tail() reads
	// only `buf` after trimming.
	n int64
}

// newRingBuffer allocates a ring buffer of the given capacity. The
// backing slice is pre-allocated to capacity so the first Write does
// not trigger a reallocation — the customer app's first stdout bytes
// land in already-allocated memory.
func newRingBuffer(capacity int) *ringBuffer {
	return &ringBuffer{
		buf: make([]byte, 0, capacity),
		cap: capacity,
	}
}

// Write implements io.Writer. The mutex protects against concurrent
// stdout + stderr goroutines spawned by cmd.Run() — a customer process
// with both pipes open can have two Write calls in flight at once.
// Without the lock, a partial-line append to `buf` would race with the
// Tail() reader's slice copy.
//
// Returns (len(p), nil) always; a Write that can't be satisfied (a panic
// on out-of-memory) propagates up and crashes the customer's exec, which
// is the right failure mode — the ring buffer is not a path that can
// fail the guest boot independently of the customer's process.
func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.cap {
		// Trim from the front: keep the last `cap` bytes. Re-slicing
		// is O(1); the discarded prefix becomes garbage and is
		// reclaimed by the GC. We deliberately don't copy into a
		// smaller buffer because the slice header still references
		// the same backing array — the next Write's append-in-place
		// grows into the same memory region. The capacity budget is
		// therefore exactly `cap` bytes regardless of n.
		r.buf = r.buf[len(r.buf)-r.cap:]
	}
	r.n += int64(len(p))
	return len(p), nil
}

// Tail returns the buffer's current contents as a string. Locks the
// mutex to get a consistent view, copies into a fresh string (the caller
// owns the result). Empty string if the buffer has never been written
// to — same shape as the empty-string sentinel the RingBufferTail
// callback used before the ring buffer shipped.
//
// The string conversion allocates once per call; for a 64 KiB buffer
// that is a 64 KiB allocation, but the characterize probe calls
// Tail() at most twice per boot (once for the bind-wait's deadline
// pass, once for the final report), so the allocation cost is bounded.
func (r *ringBuffer) Tail() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) == 0 {
		return ""
	}
	// Copy under the lock so the returned string is detached from any
	// subsequent Reset()/Write(). Without the copy, a Reset() between
	// Tail() and the caller's use of the string could see the buffer
	// mutated mid-string; the copy makes that race impossible.
	return string(r.buf)
}

// Reset drops the buffer's contents. O(1), reuses the underlying
// capacity so the next Write does not allocate. Called by the
// Supervisor on every TrackCommand (every restart starts a fresh 64 KiB
// window, per the documented contract in supervise.go's lastLog
// doc-comment).
func (r *ringBuffer) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = r.buf[:0]
	r.n = 0
}
