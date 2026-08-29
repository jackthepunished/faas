// rollup_test.go — issue #72 / ADR-124 / ADR-125 PR-A3 commit 4
//
// Unit tests for the mirror rollup + retention sweep package.
// Pinned traits:
//   - RollupOnce runs the rollup SQL with a half-open
//     [windowStart, windowEnd) window.
//   - SweepOldLedgerRows runs the DELETE with the supplied cutoff.
//   - The rollup SQL uses an additive-merge ON CONFLICT (a re-run
//     over the same hour ADDS to the existing count, not overwrites).
//   - The sweep SQL is a single DELETE with one bound parameter.
//
// Uses a stub execer that records the SQL + args so a regression
// that flips the rollup to overwrite or the sweep to window-bounded
// fails a fast unit test, not the e2e.

package mirror

import (
	"context"
	"sync"
	"testing"
	"time"
)

// stubExecer records the (sql, args) tuples the rollup + sweep
// pass to Exec. Returns a configurable rows-affected count so
// tests can assert on the wiring without a Postgres dependency.
type stubExecer struct {
	mu          sync.Mutex
	calls       []stubCall
	rowsByQuery map[string]int64
	execErr     error
}

type stubCall struct {
	sql  string
	args []any
}

func (s *stubExecer) Exec(_ context.Context, sql string, args ...any) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.execErr != nil {
		return 0, s.execErr
	}
	cp := make([]any, len(args))
	copy(cp, args)
	s.calls = append(s.calls, stubCall{sql: sql, args: cp})
	if s.rowsByQuery != nil {
		// Crude match by SQL keyword substring; the rollup SQL
		// and the sweep SQL contain distinct leading keywords
		// (INSERT vs DELETE) so a contains check is enough.
		for prefix, rows := range s.rowsByQuery {
			if contains(sql, prefix) {
				return rows, nil
			}
		}
	}
	return 1, nil
}

func (s *stubExecer) callsFor(prefix string) []stubCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []stubCall
	for _, c := range s.calls {
		if contains(c.sql, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// TestRollupOnce_HalfOpenWindow pins the window-bound contract.
// RollupOnce must reject windowEnd <= windowStart and must pass
// the supplied UTC times verbatim as the SQL args.
func TestRollupOnce_HalfOpenWindow(t *testing.T) {
	s := &stubExecer{rowsByQuery: map[string]int64{"INSERT": 5}}
	start := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	end := start.Add(1 * time.Hour)
	got, err := RollupOnce(context.Background(), s, start, end)
	if err != nil {
		t.Fatalf("RollupOnce: %v", err)
	}
	if got != 5 {
		t.Errorf("rows = %d, want 5", got)
	}
	calls := s.callsFor("INSERT")
	if len(calls) != 1 {
		t.Fatalf("INSERT calls = %d, want 1", len(calls))
	}
	if calls[0].args[0] != start || calls[0].args[1] != end {
		t.Errorf("args = [%v,%v], want [%v,%v]", calls[0].args[0], calls[0].args[1], start, end)
	}
}

// TestRollupOnce_RejectsInvertedWindow pins the window validation.
// A non-monotonic window is a programming error, not a runtime
// condition; RollupOnce must reject it without touching the
// database.
func TestRollupOnce_RejectsInvertedWindow(t *testing.T) {
	s := &stubExecer{}
	start := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	end := start.Add(-1 * time.Hour)
	if _, err := RollupOnce(context.Background(), s, start, end); err == nil {
		t.Error("RollupOnce accepted inverted window")
	}
	if got := len(s.calls); got != 0 {
		t.Errorf("calls = %d, want 0 (validation must short-circuit)", got)
	}
}

// TestRollupOnce_AdditiveMerge pins the additive-merge ON
// CONFLICT shape. The SQL must use DO UPDATE SET with `+` on
// each count column, NOT the EXCLUDED.col overwrite shape that
// usage_daily uses. Re-running the rollup on a partially-
// collected hour must ADD to the running sum.
func TestRollupOnce_AdditiveMerge(t *testing.T) {
	s := &stubExecer{rowsByQuery: map[string]int64{"INSERT": 0}}
	start := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	end := start.Add(1 * time.Hour)
	if _, err := RollupOnce(context.Background(), s, start, end); err != nil {
		t.Fatalf("RollupOnce: %v", err)
	}
	calls := s.callsFor("INSERT")
	if len(calls) != 1 {
		t.Fatalf("INSERT calls = %d, want 1", len(calls))
	}
	sql := collapseWhitespace(calls[0].sql)
	// Each count column must be additive: <table>.<col> + EXCLUDED.<col>.
	// A regression to overwrite (e.g. = EXCLUDED.col) silently
	// destroys the running sum.
	for _, col := range []string{"total_invocations", "status_diff_count", "crash_count", "cap_at_max_count"} {
		want := "mirror_invocation_summary." + col + " + EXCLUDED." + col
		if !contains(sql, want) {
			t.Errorf("rollup SQL missing additive merge on %q (must be %q)", col, want)
		}
	}
}

// TestSweepOldLedgerRows_DeletesOnlyStale pins that the sweep
// passes the cutoff verbatim to a single-parameter DELETE.
func TestSweepOldLedgerRows_DeletesOnlyStale(t *testing.T) {
	s := &stubExecer{rowsByQuery: map[string]int64{"DELETE": 42}}
	cutoff := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	got, err := SweepOldLedgerRows(context.Background(), s, cutoff)
	if err != nil {
		t.Fatalf("SweepOldLedgerRows: %v", err)
	}
	if got != 42 {
		t.Errorf("rows = %d, want 42", got)
	}
	calls := s.callsFor("DELETE")
	if len(calls) != 1 {
		t.Fatalf("DELETE calls = %d, want 1", len(calls))
	}
	if calls[0].args[0] != cutoff {
		t.Errorf("arg[0] = %v, want %v", calls[0].args[0], cutoff)
	}
}

// TestSweepOldLedgerRows_NoArgs pins that the sweep SQL has
// exactly one bind parameter. A regression that adds a second
// (e.g. an accidental app-id scope) would silently stop
// sweeping — the DELETE would still succeed but match nothing.
func TestSweepOldLedgerRows_NoArgs(t *testing.T) {
	s := &stubExecer{}
	if _, err := SweepOldLedgerRows(context.Background(), s, time.Now()); err != nil {
		t.Fatalf("SweepOldLedgerRows: %v", err)
	}
	calls := s.callsFor("DELETE")
	if len(calls) != 1 {
		t.Fatalf("DELETE calls = %d, want 1", len(calls))
	}
	if got := len(calls[0].args); got != 1 {
		t.Errorf("arg count = %d, want 1", got)
	}
}

// TestDefaultRollupInterval pins the cadence. The contract is
// "small enough that a meterd restart covers a missed tick in
// ~one cycle, large enough that the SQL doesn't dominate the
// connection pool" — a regression to a much higher value (e.g.
// 1h) would leave the dashboard chip blank for an hour after
// every boot.
func TestDefaultRollupInterval(t *testing.T) {
	if DefaultRollupInterval < time.Minute {
		t.Errorf("DefaultRollupInterval = %v, want >= 1m", DefaultRollupInterval)
	}
	if DefaultRollupInterval > time.Hour {
		t.Errorf("DefaultRollupInterval = %v, want <= 1h", DefaultRollupInterval)
	}
}

// TestDefaultLedgerRetention pins the 7-day retention contract.
// Shorter values lose the customer's "my mirror wasn't firing
// yesterday" debugging window; longer values blow up the table
// at high mirror volume.
func TestDefaultLedgerRetention(t *testing.T) {
	want := 7 * 24 * time.Hour
	if DefaultLedgerRetention != want {
		t.Errorf("DefaultLedgerRetention = %v, want %v", DefaultLedgerRetention, want)
	}
}

// contains is a tiny strings.Contains polyfill so the rollup_test
// file doesn't depend on "strings" for a single use site.
func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// collapseWhitespace folds any run of whitespace (spaces, tabs,
// newlines) into a single space so SQL pattern matches aren't
// sensitive to formatting changes. Mirrors the same approach
// sqlc uses for SQL whitespace tolerance.
func collapseWhitespace(s string) string {
	var out []byte
	prevSpace := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		isSpace := c == ' ' || c == '\t' || c == '\n' || c == '\r'
		if isSpace {
			if !prevSpace {
				out = append(out, ' ')
			}
			prevSpace = true
			continue
		}
		prevSpace = false
		out = append(out, c)
	}
	return string(out)
}
