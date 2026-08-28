// spans_accumulator_test.go — ADR-127 PR-D code-review #4.
//
// Verifies the SpansAccumulator.Add account_id re-check
// contract: every Add re-validates that the bucket's
// account_id matches the caller's account_id. A trace_id
// contended across accounts returns ErrAccountMismatch so the
// handler can 401 instead of silently overwriting.

package gateway

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestSpansAccumulator_Add_SameAccountCoalesces(t *testing.T) {
	s := NewSpansAccumulator()
	id := uuid.New()
	tid := "00000000000000000000000000000001"
	spans := []summarizedSpan{{TraceID: tid, SpanID: "aa", EndTimeUnixNano: 1}}

	n, err := s.Add(tid, id, spans)
	if err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if n != 1 {
		t.Errorf("first Add n = %d, want 1", n)
	}
	// Same trace_id, same account — must succeed (the dedupe
	// set will drop the duplicate span, but no error).
	n, err = s.Add(tid, id, nil)
	if err != nil {
		t.Fatalf("second Add: %v", err)
	}
	if n != 0 {
		t.Errorf("second Add n = %d, want 0 (dedupe)", n)
	}
}

// TestSpansAccumulator_Add_AccountMismatch is the regression
// for PR-D code-review #4. Two POSTs for the same trace_id
// but different account_ids must NOT silently coalesce — the
// second Add must return ErrAccountMismatch so the handler
// 401s. The OLD account's bucket is preserved (its legitimate
// buffered spans flush on the next tick).
func TestSpansAccumulator_Add_AccountMismatch(t *testing.T) {
	s := NewSpansAccumulator()
	acctA := uuid.New()
	acctB := uuid.New()
	tid := "00000000000000000000000000000002"

	if _, err := s.Add(tid, acctA, []summarizedSpan{{TraceID: tid, SpanID: "aa", EndTimeUnixNano: 1}}); err != nil {
		t.Fatalf("first Add(acctA): %v", err)
	}
	_, err := s.Add(tid, acctB, []summarizedSpan{{TraceID: tid, SpanID: "bb", EndTimeUnixNano: 1}})
	if !errors.Is(err, ErrAccountMismatch) {
		t.Fatalf("second Add(acctB) err = %v, want ErrAccountMismatch", err)
	}
	// The OLD account's bucket is intact — a follow-up Add
	// from acctA still coalesces normally.
	if _, err := s.Add(tid, acctA, []summarizedSpan{{TraceID: tid, SpanID: "cc", EndTimeUnixNano: 2}}); err != nil {
		t.Fatalf("third Add(acctA): %v", err)
	}
}
