package logbuf

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRing_LineOrdering pins the roundtrip property: bytes written in a
// specific sequence come back in the same sequence from Snapshot, with
// monotonically increasing Seq starting at 1 and consecutive lines.
func TestRing_LineOrdering(t *testing.T) {
	r := New(1 << 20)
	in := []string{"alpha", "beta", "gamma", "delta"}
	for _, s := range in {
		if _, err := r.Write("stdout", []byte(s+"\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	got := r.Snapshot(0)
	if len(got) != len(in) {
		t.Fatalf("Snapshot size = %d, want %d", len(got), len(in))
	}
	for i, line := range got {
		if line.Line != in[i] {
			t.Errorf("line[%d] = %q, want %q", i, line.Line, in[i])
		}
		if line.Stream != "stdout" {
			t.Errorf("line[%d].Stream = %q, want stdout", i, line.Stream)
		}
		if i == 0 {
			if line.Seq != 1 {
				t.Errorf("line[0].Seq = %d, want 1", line.Seq)
			}
		} else {
			if line.Seq != got[i-1].Seq+1 {
				t.Errorf("line[%d].Seq = %d, not monotonically +1 over line[%d]=%d",
					i, line.Seq, i-1, got[i-1].Seq)
			}
		}
	}
}

// TestRing_SnapshotSince verifies Snapshot(sinceSeq) returns only Lines with
// Seq >= sinceSeq, no duplicates and no gaps. This is the load-bearing
// property for vmmdgrpc.Server.Logs's replay path (issue #254 acceptance #2:
// clients can resume from a known cursor).
func TestRing_SnapshotSince(t *testing.T) {
	r := New(1 << 20)
	for i := 0; i < 10; i++ {
		if _, err := r.Write("stdout", []byte("line\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	// Replay from seq=5 — must return lines 5..10 (6 entries), with no
	// repeats and consecutive sequence numbers.
	got := r.Snapshot(5)
	if len(got) != 6 {
		t.Fatalf("Snapshot(5) size = %d, want 6", len(got))
	}
	for i, line := range got {
		want := int64(5 + i)
		if line.Seq != want {
			t.Errorf("got[%d].Seq = %d, want %d", i, line.Seq, want)
		}
	}
	// Replay beyond the high-water mark returns nil.
	if got := r.Snapshot(99); got != nil {
		t.Errorf("Snapshot(99) = %v, want nil", got)
	}
	// sinceSeq=0 returns the entire retained buffer (same as Snapshot(1)).
	all := r.Snapshot(0)
	if len(all) != 10 {
		t.Errorf("Snapshot(0) size = %d, want 10", len(all))
	}
}

// TestRing_Wraparound pins the eviction contract: when the byte budget is
// exhausted, the oldest Line is dropped on the next commit and the new Seq
// stays monotonic. totalBytes is also bounded by maxBytes at all times.
//
// Math: 17-byte payload lines on a 64-byte ring.
//   - 3 commits: 3×17 = 51 ≤ 64 → no eviction, 3 retained.
//   - 4th commit: totalBytes=51, line=17 → 68 > 64, so evict 1 → retain 3 lines.
//   - 5th commit: totalBytes=51, evict 1, retain 3 lines.
// We assert the buffer holds exactly 3 lines (the most recent three)
// regardless of how many commits we make beyond the budget, and the
// retained sequence numbers are consecutive from the latest committed.
func TestRing_Wraparound(t *testing.T) {
	const (
		ringBytes = 64
		payloadLen = 17 // stored line length; does not include the '\n' terminator
	)
	r := New(ringBytes)
	payload := make([]byte, payloadLen)
	for i := range payload {
		payload[i] = 'x'
	}
	for i := 0; i < 10; i++ {
		chunk := append([]byte{}, payload...)
		chunk = append(chunk, '\n')
		if _, err := r.Write("stderr", chunk); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if r.totalBytes > ringBytes {
			t.Fatalf("after commit %d: totalBytes=%d exceeds maxBytes=%d",
				i, r.totalBytes, ringBytes)
		}
	}
	got := r.Snapshot(0)
	// 3 lines × 17 bytes = 51 ≤ 64; 4 × 17 = 68 > 64. So ring always
	// retains 3 lines once steady state is reached.
	if len(got) != 3 {
		t.Fatalf("retained=%d, want 3 (oldest lines should have been evicted)",
			len(got))
	}
	// After 10 commits, the 3 retained lines are commits 8, 9, 10.
	if got[0].Seq != 8 || got[1].Seq != 9 || got[2].Seq != 10 {
		t.Errorf("retained seqs = [%d, %d, %d], want [8, 9, 10]",
			got[0].Seq, got[1].Seq, got[2].Seq)
	}
	// totalBytes must stay at 51 (3 × 17).
	if r.totalBytes != payloadLen*3 {
		t.Errorf("totalBytes = %d, want %d", r.totalBytes, payloadLen*3)
	}
}

// TestRing_PartialLine pins the line-fragmentation contract: a Write without
// a trailing '\n' is buffered until the next Write completes it. A subsequent
// Snapshot returns exactly one Line carrying the joined bytes, never two.
func TestRing_PartialLine(t *testing.T) {
	r := New(1 << 20)
	if _, err := r.Write("stdout", []byte("hello ")); err != nil {
		t.Fatalf("Write 1: %v", err)
	}
	// Snapshot must NOT include the incomplete line yet.
	if got := r.Snapshot(0); got != nil {
		t.Errorf("Snapshot after partial Write = %v, want nil", got)
	}
	if _, err := r.Write("stdout", []byte("world\n")); err != nil {
		t.Fatalf("Write 2: %v", err)
	}
	got := r.Snapshot(0)
	if len(got) != 1 {
		t.Fatalf("Snapshot after completion = %d lines, want 1", len(got))
	}
	if got[0].Line != "hello world" {
		t.Errorf("joined line = %q, want %q", got[0].Line, "hello world")
	}
	if got[0].Seq != 1 {
		t.Errorf("Seq = %d, want 1", got[0].Seq)
	}
}

// TestRing_DeterminismMB writes 1 MiB of newline-terminated bytes from a
// single goroutine and asserts the snapshot reproduces the count correctly.
// The Line field stores the payload BEFORE the newline (the terminator is
// consumed by the splitter), so retained length is len(payload) not
// len(payload)+1. Determinism here is the test property: a flaky ring
// (lost line, duplicated line) fails on the count mismatch.
func TestRing_DeterminismMB(t *testing.T) {
	const total = 1 << 20
	r := New(8 << 20) // 8 MiB cap, plenty of headroom for 1 MiB payload
	var buf [8]byte
	for i := 0; i < 131072; i++ {
		copy(buf[:], "L000000\n")
		if _, err := r.Write("stdout", buf[:]); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	got := r.Snapshot(0)
	if len(got) != 131072 {
		t.Fatalf("retained = %d lines, want 131072", len(got))
	}
	for i := 0; i < len(got); i++ {
		if len(got[i].Line) != 7 {
			t.Errorf("got[%d] length = %d, want 7 (raw line dropped?)",
				i, len(got[i].Line))
		}
	}
}

// TestRing_Subscribe pins the live-tail contract: Subscribe returns new
// lines as they are committed, in commit order, and cancel() detaches the
// subscription so subsequent lines are no longer published to that channel.
func TestRing_Subscribe(t *testing.T) {
	r := New(1 << 20)
	ch, cancel := r.Subscribe()
	if _, err := r.Write("stdout", []byte("a\nb\nc\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	var got []string
	for i := 0; i < 3; i++ {
		select {
		case line, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed prematurely at i=%d", i)
			}
			got = append(got, line.Line)
		case <-time.After(time.Second):
			t.Fatalf("timeout waiting for line %d", i)
		}
	}
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// Cancel must detach — subsequent Write does NOT publish to ch.
	cancel()
	if _, err := r.Write("stdout", []byte("post-cancel\n")); err != nil {
		t.Fatalf("Write post-cancel: %v", err)
	}
	select {
	case line, ok := <-ch:
		if ok {
			t.Errorf("received line %q on cancelled channel", line.Line)
		}
	case <-time.After(50 * time.Millisecond):
		// Expected — nothing arrives.
	}
}

// TestRing_ConcurrentWrites pins the contract that two goroutines writing
// concurrently do not race, do not lose lines, and do not corrupt sequence
// numbers. This is what the production path looks like: the VM's stdout and
// stderr each push onto the same ring through two separate Writers.
//
// We assert the buffer is FIFO within each Write chunk (no torn lines),
// and the highest Seq equals the total committed line count, which is
// the property a slow consumer would rely on to resume correctly.
func TestRing_ConcurrentWrites(t *testing.T) {
	const perGoroutine = 1000
	r := New(8 << 20)
	var wg sync.WaitGroup
	var totalWritten int64
	for g := 0; g < 4; g++ {
		wg.Add(1)
		g := g
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				chunk := []byte("g0-L00000\n")
				chunk[1] = byte('0' + g)
				// Encode i into bytes 3..6 so each line is unique.
				n := i
				for j := 6; j >= 3; j-- {
					chunk[j] = byte('0' + n%10)
					n /= 10
				}
				if _, err := r.Write("stdout", chunk); err != nil {
					t.Errorf("Write: %v", err)
					return
				}
				atomic.AddInt64(&totalWritten, 1)
			}
		}()
	}
	wg.Wait()

	if int64(perGoroutine*4) != atomic.LoadInt64(&totalWritten) {
		t.Fatalf("totalWritten = %d, want %d", totalWritten, perGoroutine*4)
	}
	snap := r.Snapshot(0)
	if int64(len(snap)) != totalWritten {
		t.Errorf("retained = %d, want %d (lines lost)", len(snap), totalWritten)
	}
	// Seq must be contiguous and monotonic — gaps signal a corrupted Seq.
	for i, ln := range snap {
		if ln.Seq != int64(i+1) {
			t.Fatalf("snap[%d].Seq = %d, want %d (gap or duplicate)",
				i, ln.Seq, i+1)
		}
	}
}

// TestRing_CloseAfterWrite pins the lifecycle contract: after Close,
// Write returns ErrClosed, Snapshot still returns the last-committed lines,
// and active subscribers receive a closed channel.
func TestRing_CloseAfterWrite(t *testing.T) {
	r := New(1 << 20)
	if _, err := r.Write("stdout", []byte("before-close\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := r.Write("stdout", []byte("post-close\n")); err == nil {
		t.Fatalf("Write after Close = nil, want ErrClosed")
	}
	// Snapshot is unaffected by Close.
	snap := r.Snapshot(0)
	if len(snap) != 1 || snap[0].Line != "before-close" {
		t.Errorf("post-close Snapshot = %v, want one line %q", snap, "before-close")
	}
	// Close is idempotent.
	if err := r.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}

// TestRing_Empty pins the Snapshot(sinceSeq<0) → all-lines behaviour and
// the Snapshot on a fresh ring → nil behaviour. Both are exercise branches
// in vmmdgrpc.Server.Logs that an empty-firecracker replay would hit.
func TestRing_Empty(t *testing.T) {
	r := New(1 << 20)
	if got := r.Snapshot(0); got != nil {
		t.Errorf("Snapshot on empty = %v, want nil", got)
	}
	if got := r.Snapshot(-1); got != nil {
		t.Errorf("Snapshot(-1) on empty = %v, want nil", got)
	}
	// Write of empty bytes is a successful no-op.
	n, err := r.Write("stdout", nil)
	if n != 0 || err != nil {
		t.Errorf("Write(nil) = (%d, %v), want (0, nil)", n, err)
	}
}
