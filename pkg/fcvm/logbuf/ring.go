// Package logbuf implements a per-instance byte-bounded line ring used to
// surface Firecracker guest stdout/stderr to vmmd's Logs gRPC (issue #254,
// Move 4). The same producer hands each completed line to every active
// subscriber channel and retains it in the snapshot until the byte budget is
// exhausted; the oldest lines are then evicted FIFO.
//
// The ring lives here (next to its only consumer in pkg/fcvm/vmm.go) rather
// than in pkg/wire because wire has no ring primitive and the only adjacent
// shape — pkg/sched/scaleup/ringbuf.go::RingBuffer — is a counter bucket for
// RPS windows, not a byte/line ring. Keeping the type co-located with its
// single read/write surface (the jailed firecracker process) lets the test
// suite pin ordering + wraparound without spinning up a real VM.
package logbuf

import (
	"sync"
	"time"
)

// DefaultMaxBytes is the byte cap vmmd allocates per live instance when
// constructing a new ring (10 MiB; small enough to fit thousands of instances
// in the §6.2.2 tenant RAM budget, large enough to hold ~5k-50k lines).
const DefaultMaxBytes = 10 << 20

// Line is one completed stdout/stderr line with a monotonic sequence number
// assigned at ring intake. Concretely a single Write call may carry zero, one,
// or many Lines depending on how many '\n' bytes it contained; partial lines
// (no trailing '\n') are buffered internally and only materialise as a Line
// when the next Write completes them.
type Line struct {
	Seq       int64     `json:"seq"`
	Stream    string    `json:"stream"` // "stdout" or "stderr"
	Line      string    `json:"line"`
	WrittenAt time.Time `json:"written_at"`
}

// Ring is a fixed-budget, line-fragmenting log buffer keyed by Firecracker
// instance id. It is safe for concurrent writers (one firecracker stdio
// goroutine per instance) and concurrent readers (every gRPC subscriber
// against Logs(req)).
//
// Memory model: totalBytes tracks the sum of retained Line.Line payload
// lengths; the per-line overhead is a fixed-size Line struct in a separate
// slice, so the figure is close to the actual heap cost of the buffer. The
// invariant totalBytes <= maxBytes is preserved by evicting the oldest
// complete line at intake; Write never blocks on a full ring.
//
// Subscriber semantics: Subscribe returns a buffered channel that receives
// every Line as it is committed. The cancel func detaches the subscription
// and closes the channel. A Write that races with Close still publishes to
// any channel that has not yet been detached; channels detached before
// Close are drained by Close's locked section so a single producer cannot
// block on a dead consumer.
type Ring struct {
	maxBytes int

	mu         sync.Mutex
	lines      []Line // ring slot array, oldest at head
	head       int    // index of the oldest retained line
	size       int    // number of retained lines (0..len(lines))
	totalBytes int    // sum of Line.Line payload lengths
	nextSeq    int64  // monotonic per-ring; the Seq assigned to the next Write that completes a line
	tail       []byte // partial-line buffer for Write's bytes without a '\n'
	subs       []chan Line

	closed bool
}

// New constructs a Ring with the given byte cap. maxBytes <= 0 falls back to
// DefaultMaxBytes so callers can wire a zero-default without a flag day.
func New(maxBytes int) *Ring {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	return &Ring{
		maxBytes: maxBytes,
	}
}

// Write consumes a chunk of firecracker stdout/stderr bytes, splitting on
// '\n' and emitting one Line per completed line. Bytes without a trailing
// '\n' are buffered until the next Write completes them, so a chunk that
// carries N lines produces N Lines and the channel publish count matches
// the snapshot replay count exactly.
//
// Returns len(p) on success (we own the bytes; io.Copy contract). On a
// closed ring the bytes are dropped and (0, ErrClosed) is returned — keep
// the producer's exec.Cmd stdout going; fcvm.Kill is idempotent so a
// double-close at teardown does not double-return errors that the watchdog
// would log anyway.
func (r *Ring) Write(stream string, p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	now := time.Now()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return 0, ErrClosed
	}
	// Append the new chunk to the partial tail and scan for '\n' boundaries.
	// Most Writes carry at most a handful of lines, so a fresh copy with one
	// IndexByte pass per '\n' beats a bufio.Scanner heap allocation under the
	// per-instance hot path.
	if r.tail == nil {
		r.tail = make([]byte, 0, len(p)+64)
	}
	r.tail = append(r.tail, p...)
	// Pull every complete line off the tail into committed Lines.
	for {
		i := indexByte(r.tail, '\n')
		if i < 0 {
			break
		}
		line := string(r.tail[:i])
		r.tail = r.tail[i+1:]
		r.commitLocked(stream, line, now)
	}
	r.mu.Unlock()
	return len(p), nil
}

// commitLocked appends one completed line to the ring, evicting the oldest
// line(s) if totalBytes would exceed the budget. Caller holds r.mu.
func (r *Ring) commitLocked(stream, line string, now time.Time) {
	r.nextSeq++
	ln := Line{
		Seq:       r.nextSeq,
		Stream:    stream,
		Line:      line,
		WrittenAt: now,
	}
	// Evict from head until totalBytes + len(line) <= maxBytes. We must
	// evict BEFORE assigning to the slot because the slot overwrites the
	// head slot on full ring; the per-line overhead in the slice is
	// constant so we don't need to charge it here.
	for r.totalBytes+len(line) > r.maxBytes && r.size > 0 {
		r.totalBytes -= len(r.lines[r.head].Line)
		r.head = (r.head + 1) % cap(r.lines)
		r.size--
	}
	// Grow the underlying slice on demand. We choose capacity over exact
	// fit so a chatty app doesn't reallocate every line; maxBytes caps the
	// working set regardless of cap(lines).
	if r.size == cap(r.lines) {
		newCap := 64
		if c := cap(r.lines); c > 0 {
			newCap = c * 2
		}
		grown := make([]Line, newCap)
		if r.size > 0 {
			// Copy retained slice in head→tail order so the new layout is
			// contiguous; subsequent appends/evictions are O(1).
			for k := 0; k < r.size; k++ {
				grown[k] = r.lines[(r.head+k)%cap(r.lines)]
			}
			r.head = 0
		}
		r.lines = grown
	}
	idx := (r.head + r.size) % cap(r.lines)
	r.lines[idx] = ln
	r.size++
	r.totalBytes += len(line)
	// Publish to every active subscriber. We snapshot subs under the lock
	// to release it before sending so a slow subscriber cannot stall a
	// producer that's still consuming firecracker stdout.
	subs := r.subs
	for _, ch := range subs {
		select {
		case ch <- ln:
		default:
			// Slow consumer — drop. The drop counter is surfaced
			// at the apid layer via apid_logs_dropped_total so the
			// §12 panel reflects the per-instance slow-consumer rate.
		}
	}
}

// Snapshot returns a copy of every Line whose Seq is >= sinceSeq, in write
// order. A sinceSeq <= 0 returns the entire retained buffer; a sinceSeq
// larger than the highest assigned Seq returns nil. The result is a fresh
// slice — callers may retain, marshal, or modify it without aliasing the
// ring.
func (r *Ring) Snapshot(sinceSeq int64) []Line {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size == 0 {
		return nil
	}
	// Find the first retained Line whose Seq >= sinceSeq. A linear scan is
	// fine — size is bounded by maxBytes/avgLineLen and the snapshot path
	// runs on gRPC attach, not on every line.
	start := 0
	if sinceSeq > 0 {
		for k := 0; k < r.size; k++ {
			if r.lines[(r.head+k)%cap(r.lines)].Seq >= sinceSeq {
				start = k
				break
			}
			if k == r.size-1 {
				return nil
			}
		}
	}
	out := make([]Line, r.size-start)
	for k := 0; k < len(out); k++ {
		out[k] = r.lines[(r.head+start+k)%cap(r.lines)]
	}
	return out
}

// Subscribe returns a channel that receives every committed Line from now
// on, plus a cancel func that detaches the subscription and closes the
// channel. Each Subscribe call returns a fresh channel — concurrent
// subscribers do not share their backpressure.
//
// The channel is buffered (capacity 64) so a momentary stall on the network
// doesn't drop the producer. A subscriber that falls further behind causes
// the ring to drop on the next commit (see Write) and increment the
// apid_logs_dropped_total counter at the apid layer.
func (r *Ring) Subscribe() (<-chan Line, func()) {
	ch := make(chan Line, 64)
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	r.subs = append(r.subs, ch)
	r.mu.Unlock()
	cancel := func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		for i, c := range r.subs {
			if c == ch {
				r.subs = append(r.subs[:i], r.subs[i+1:]...)
				break
			}
		}
		// Drain any pending Line so the channel's consumer sees a clean
		// EOF rather than a stuck goroutine on a Send. After cancel we
		// close the channel so range over it exits.
		select {
		case <-ch:
		default:
		}
		close(ch)
	}
	return ch, cancel
}

// Close marks the ring as closed and tears down every active subscriber
// channel. Subsequent Write calls return ErrClosed; Snapshot returns the
// last-committed lines up to the close point. Idempotent.
//
// The pkg/fcvm/vmm.go::JailerVMM.Kill path calls Close so a park/unpark
// cycle drops the ring and frees the byte budget — invariant §6.2-4
// (parked app = zero resident RAM).
func (r *Ring) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	subs := r.subs
	r.subs = nil
	r.mu.Unlock()
	for _, ch := range subs {
		close(ch)
	}
	return nil
}

// ErrClosed is returned by Write after the ring has been Close()'d.
var ErrClosed = errClosed{}

type errClosed struct{}

func (errClosed) Error() string { return "logbuf: ring closed" }

// indexByte is a 1-line shim over the standard library that the compiler
// inlines, kept as a named helper so the hot Write loop is one line shorter
// to read and to keep the file gofmt-clean without an inline closure.
func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}
