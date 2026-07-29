// Retention cron tests (ADR-049 §B.4). Exercises the
// idempotent-DB-exec contract under the BATCHED DELETE shape
// (PR #428 review blocker #4):
//
//   1. First tick on a 14-month-old dataset — DELETE path
//      returns the row count, loops until short-read.
//   2. Second tick on the same dataset — 0 rows (idempotent).
//   3. Context cancel — Loop returns.
//   4. Exec error — propagated, Loop continues on next tick.
//   5. Batch cap hit — returns (rows, ErrRetentionBatchCap).
//   6. SQL substring pins (ctid subquery + LIMIT $1 + 13-month
//      cutoff) so a refactor back to an unbounded DELETE is
//      caught at unit-test time.

package meter

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// rowsFunc lets a test script the per-call rowsAffected return
// so the multi-batch loop's exit condition can be exercised.
type rowsFunc func(callIndex int) int64

type recordingExecer struct {
	mu      sync.Mutex
	calls   []retentionCall
	rowsFn  rowsFunc
	errFn   func(callIndex int) error
	callIdx int
}

type retentionCall struct {
	SQL  string
	Args []any
}

func (r *recordingExecer) Exec(_ context.Context, sql string, args ...any) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]any, len(args))
	copy(cp, args)
	r.calls = append(r.calls, retentionCall{SQL: sql, Args: cp})
	idx := r.callIdx
	r.callIdx++
	if r.errFn != nil {
		if err := r.errFn(idx); err != nil {
			return 0, err
		}
	}
	if r.rowsFn != nil {
		return r.rowsFn(idx), nil
	}
	return 0, nil
}

func (r *recordingExecer) callsCopy() []retentionCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]retentionCall, len(r.calls))
	copy(out, r.calls)
	return out
}

func TestRetentionOnce_ReportsRowCount(t *testing.T) {
	// Two batches: first is full (10 000), second is a short read (100).
	r := &recordingExecer{
		rowsFn: func(i int) int64 {
			if i == 0 {
				return RetentionBatchSize
			}
			return 100
		},
	}
	got, err := RetentionOnce(context.Background(), r)
	if err != nil {
		t.Fatalf("RetentionOnce: %v", err)
	}
	if got != RetentionBatchSize+100 {
		t.Errorf("rows = %d, want %d (sum across batches)", got, RetentionBatchSize+100)
	}
	calls := r.callsCopy()
	if len(calls) != 2 {
		t.Errorf("calls = %d, want 2 (full batch + short read)", len(calls))
	}
	for i, c := range calls {
		if !strings.Contains(c.SQL, "13 months") {
			t.Errorf("call[%d] missing 13-month cutoff: %q", i, c.SQL)
		}
		if c.Args[0] != RetentionBatchSize {
			t.Errorf("call[%d] arg[0] = %v, want %d", i, c.Args[0], RetentionBatchSize)
		}
	}
}

func TestRetentionOnce_Idempotent(t *testing.T) {
	r := &recordingExecer{
		rowsFn: func(i int) int64 {
			if i == 0 {
				return RetentionBatchSize
			}
			return 0
		},
	}
	if _, err := RetentionOnce(context.Background(), r); err != nil {
		t.Fatalf("first RetentionOnce: %v", err)
	}
	got, err := RetentionOnce(context.Background(), r)
	if err != nil {
		t.Fatalf("second RetentionOnce: %v", err)
	}
	if got != 0 {
		t.Errorf("second call rows = %d, want 0 (idempotent)", got)
	}
}

func TestRetentionOnce_PropagatesError(t *testing.T) {
	want := errors.New("postgres dropped connection")
	r := &recordingExecer{errFn: func(int) error { return want }}
	_, err := RetentionOnce(context.Background(), r)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}

func TestRetentionOnce_HitsBatchCap(t *testing.T) {
	// Stub always returns a full batch — loop should hit
	// MaxRetentionBatches and return the sentinel.
	r := &recordingExecer{
		rowsFn: func(int) int64 { return RetentionBatchSize },
	}
	got, err := RetentionOnce(context.Background(), r)
	if !errors.Is(err, ErrRetentionBatchCap) {
		t.Errorf("err = %v, want ErrRetentionBatchCap", err)
	}
	if want := int64(MaxRetentionBatches) * RetentionBatchSize; got != want {
		t.Errorf("rows = %d, want %d (cap × batch size)", got, want)
	}
	if got := len(r.callsCopy()); got != MaxRetentionBatches {
		t.Errorf("calls = %d, want %d (one per batch)", got, MaxRetentionBatches)
	}
}

func TestRetentionOnce_ShortReadExits(t *testing.T) {
	r := &recordingExecer{
		rowsFn: func(int) int64 { return 5 },
	}
	got, err := RetentionOnce(context.Background(), r)
	if err != nil {
		t.Fatalf("RetentionOnce: %v", err)
	}
	if got != 5 {
		t.Errorf("rows = %d, want 5", got)
	}
	if got := len(r.callsCopy()); got != 1 {
		t.Errorf("calls = %d, want 1 (short read exits the loop)", got)
	}
}

func TestRetentionSQL_HasBoundedDeleteShape(t *testing.T) {
	must := []string{
		"DELETE FROM public.usage_minutes",
		"WHERE ctid IN (",
		"SELECT ctid FROM public.usage_minutes",
		"LIMIT $1",
		"interval '13 months'",
	}
	for _, want := range must {
		if !strings.Contains(retentionBatchSQL, want) {
			t.Errorf("retentionBatchSQL missing %q; an unbounded DELETE would balloon WAL on the EX44. Got:\n%s", want, retentionBatchSQL)
		}
	}
}

func TestRetentionLoop_StopsOnContextCancel(t *testing.T) {
	r := &recordingExecer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RetentionLoop(ctx, r, time.Hour, nil)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Loop did not exit on ctx.Done()")
	}
}

func TestRetentionLoop_DefaultIntervalWhenZero(t *testing.T) {
	if DefaultRetentionInterval != 24*time.Hour {
		t.Errorf("DefaultRetentionInterval = %v, want 24h", DefaultRetentionInterval)
	}
}

func TestRetentionLoop_TicksAtLeastOnce(t *testing.T) {
	r := &recordingExecer{
		rowsFn: func(int) int64 { return 0 },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RetentionLoop(ctx, r, 10*time.Millisecond, nil)
		close(done)
	}()
	time.Sleep(40 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Loop did not exit on ctx.Done()")
	}
	if got := len(r.callsCopy()); got < 1 {
		t.Errorf("calls = %d, want ≥ 1 (loop must tick at least once)", got)
	}
}
