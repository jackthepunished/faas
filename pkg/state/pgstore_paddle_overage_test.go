package state_test

import (
	"errors"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/state"
)

// TestPg_PaddleOverageMonth_FirstRecordSucceeds pins the basic contract:
// a fresh (account, month) record returns nil and the subsequent Has
// returns true. This is the foundation the meterd pusher loop depends
// on — the gate must observe the row after RecordPaddleOverageMonth.
func TestPg_PaddleOverageMonth_FirstRecordSucceeds(t *testing.T) {
	s, ctx := pgStore(t)
	acctID, _, _ := seedLiveDeploy(t, s, ctx)
	month := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	if err := s.RecordPaddleOverageMonth(ctx, acctID, month); err != nil {
		t.Fatalf("first record: %v", err)
	}
	has, err := s.HasPaddleOverageMonth(ctx, acctID, month)
	if err != nil {
		t.Fatalf("has: %v", err)
	}
	if !has {
		t.Fatal("HasPaddleOverageMonth after Record = false, want true")
	}
}

// TestPg_PaddleOverageMonth_RecordIsIdempotent confirms two records on
// the same (account, month) both return nil and Has stays true. The
// underlying insert is `ON CONFLICT DO NOTHING`; the API surface
// promises no error leaks (no `state.ErrConflict`). Mirrors the
// AppendUsage contract test at pgstore_append_usage_test.go:103.
func TestPg_PaddleOverageMonth_RecordIsIdempotent(t *testing.T) {
	s, ctx := pgStore(t)
	acctID, _, _ := seedLiveDeploy(t, s, ctx)
	month := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	if err := s.RecordPaddleOverageMonth(ctx, acctID, month); err != nil {
		t.Fatalf("first record: %v", err)
	}
	if err := s.RecordPaddleOverageMonth(ctx, acctID, month); err != nil {
		t.Fatalf("redelivered record returned error: %v", err)
	}
	has, err := s.HasPaddleOverageMonth(ctx, acctID, month)
	if err != nil {
		t.Fatalf("has: %v", err)
	}
	if !has {
		t.Fatal("HasPaddleOverageMonth after two records = false, want true")
	}
}

// TestPg_PaddleOverageMonth_DifferentMonthsIsolated asserts a record
// for one month does not stamp the dedupe for a different month —
// the PK (account_id, month) is the composite key, not just account_id.
// Catches a regression that overloads the dedupe row.
func TestPg_PaddleOverageMonth_DifferentMonthsIsolated(t *testing.T) {
	s, ctx := pgStore(t)
	acctID, _, _ := seedLiveDeploy(t, s, ctx)

	july := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	august := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	if err := s.RecordPaddleOverageMonth(ctx, acctID, july); err != nil {
		t.Fatalf("record july: %v", err)
	}
	hasJuly, err := s.HasPaddleOverageMonth(ctx, acctID, july)
	if err != nil {
		t.Fatalf("has july: %v", err)
	}
	hasAug, err := s.HasPaddleOverageMonth(ctx, acctID, august)
	if err != nil {
		t.Fatalf("has august: %v", err)
	}
	if !hasJuly {
		t.Error("july should be stamped after Record")
	}
	if hasAug {
		t.Error("august should NOT be stamped (different month)")
	}
}

// TestPg_PaddleOverageMonth_NoUniqueViolationReturned locks down the
// API contract: 50 same-key records must all return nil and never
// surface a unique-violation error. Mirrors
// TestPg_AppendUsage_NoUniqueViolationReturned at
// pgstore_append_usage_test.go:103. Without the ON CONFLICT DO NOTHING
// this would leak state.ErrConflict on every redelivered push.
func TestPg_PaddleOverageMonth_NoUniqueViolationReturned(t *testing.T) {
	s, ctx := pgStore(t)
	acctID, _, _ := seedLiveDeploy(t, s, ctx)
	month := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	for i := 0; i < 50; i++ {
		if err := s.RecordPaddleOverageMonth(ctx, acctID, month); err != nil {
			if errors.Is(err, state.ErrConflict) {
				t.Fatalf("call %d returned ErrConflict: %v", i, err)
			}
			t.Fatalf("call %d returned error: %v", i, err)
		}
	}
}
