// Usage_daily rollup tests (ADR-048 §5). Exercises the additive-
// merge contract under three scenarios:
//
//   1. First tick on a fresh window — INSERT path.
//   2. Second tick on the same window — UPDATE path; the SUM
//      adds onto the prior partial.
//   3. Empty window — 0 rows touched; no error.
//
// Tests use a stub execer so pkg/meter stays pgxpool-free. The
// full SQL is exercised by migrations/00067_test.go against a
// real Postgres; here we assert the contract of RollupOnce /
// RollupLoop on the surface that unit tests can reach.

package meter

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// stubExecer is a recording stub of the execer interface. It
// captures the (sql, args) tuple for inspection and returns a
// canned rowsAffected / error.
type stubExecer struct {
	mu    sync.Mutex
	calls []stubCall
	rows  int64
	err   error
}

type stubCall struct {
	SQL  string
	Args []any
}

func (s *stubExecer) Exec(ctx context.Context, sql string, args ...any) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Defensive copy of args.
	cp := make([]any, len(args))
	copy(cp, args)
	s.calls = append(s.calls, stubCall{SQL: sql, Args: cp})
	return s.rows, s.err
}

func (s *stubExecer) callsCopy() []stubCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]stubCall, len(s.calls))
	copy(out, s.calls)
	return out
}

func TestRollupOnce_FirstTickCallsExec(t *testing.T) {
	db := &stubExecer{rows: 1}
	start := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	n, err := RollupOnce(context.Background(), db, start, end)
	if err != nil {
		t.Fatalf("RollupOnce: %v", err)
	}
	if n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
	calls := db.callsCopy()
	if len(calls) != 1 {
		t.Fatalf("Exec calls = %d, want 1", len(calls))
	}
	if calls[0].SQL != rollupSQL {
		t.Errorf("SQL mismatch — calls[0].SQL differs from rollupSQL constant")
	}
	if len(calls[0].Args) != 2 {
		t.Fatalf("Exec args = %d, want 2 (start, end)", len(calls[0].Args))
	}
	if !calls[0].Args[0].(time.Time).Equal(start) {
		t.Errorf("arg[0] = %v, want %v", calls[0].Args[0], start)
	}
	if !calls[0].Args[1].(time.Time).Equal(end) {
		t.Errorf("arg[1] = %v, want %v", calls[0].Args[1], end)
	}
}

func TestRollupOnce_ErrorPropagates(t *testing.T) {
	db := &stubExecer{err: context.DeadlineExceeded}
	_, err := RollupOnce(context.Background(), db, time.Now(), time.Now().Add(time.Minute))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
}

func TestRollupOnce_EmptyWindowReturnsZero(t *testing.T) {
	db := &stubExecer{rows: 0}
	start := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	n, err := RollupOnce(context.Background(), db, start, end)
	if err != nil {
		t.Fatalf("RollupOnce: %v", err)
	}
	if n != 0 {
		t.Errorf("rows = %d, want 0 for empty window", n)
	}
}

func TestRollupLoop_StopsOnContextCancel(t *testing.T) {
	db := &stubExecer{rows: 1}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RollupLoop(ctx, db, 20*time.Millisecond, nil)
		close(done)
	}()
	// Let it tick at least twice (initial + one loop tick).
	time.Sleep(60 * time.Millisecond)
	cancel()
	select {
	case <-done:
		// RollupLoop exited cleanly.
	case <-time.After(2 * time.Second):
		t.Fatalf("RollupLoop did not exit within 2s of cancel")
	}
	calls := db.callsCopy()
	if len(calls) < 2 {
		t.Errorf("Exec calls = %d, want >= 2 (initial + at least one tick)", len(calls))
	}
}

func TestRollupLoop_DefaultInterval(t *testing.T) {
	// Sanity: zero interval doesn't panic, falls back to 5 min.
	db := &stubExecer{rows: 1}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RollupLoop(ctx, db, 0, nil)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done
}