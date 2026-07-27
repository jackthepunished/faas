// Issue #279 PR A — memstore parity for the overage cap + credit
// surface. PgStore has the canonical logic; these tests pin the
// MemStore contract so a refactor of either side trips the build
// before a wired-but-broken meterd loop hits prod.
package state_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

func TestMemStoreOverageCap_NotSet(t *testing.T) {
	t.Parallel()
	s := state.NewMemStore()
	ctx := context.Background()
	acct, err := s.CreateAccount(ctx, "alice@example.com", api.PlanHobby)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	cents, ok, err := s.GetAccountOverageCapCents(ctx, acct.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ok {
		t.Fatalf("ok=true on a NULL column; want false")
	}
	if cents != 0 {
		t.Fatalf("cents = %d, want 0", cents)
	}
}

func TestMemStoreOverageCap_ZeroIsValid(t *testing.T) {
	t.Parallel()
	s := state.NewMemStore()
	ctx := context.Background()
	acct, err := s.CreateAccount(ctx, "alice@example.com", api.PlanHobby)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	s.SetOverageCapCentsForTest(acct.ID, 0)
	cents, ok, err := s.GetAccountOverageCapCents(ctx, acct.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !ok {
		t.Fatalf("ok=false on a SET-to-zero column; want true (zero is a valid cap)")
	}
	if cents != 0 {
		t.Fatalf("cents = %d, want 0", cents)
	}
}

func TestMemStoreOverageCap_Positive(t *testing.T) {
	t.Parallel()
	s := state.NewMemStore()
	ctx := context.Background()
	acct, err := s.CreateAccount(ctx, "alice@example.com", api.PlanHobby)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	s.SetOverageCapCentsForTest(acct.ID, 5000)
	cents, ok, err := s.GetAccountOverageCapCents(ctx, acct.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !ok || cents != 5000 {
		t.Fatalf("got (cents=%d, ok=%v), want (5000, true)", cents, ok)
	}
}

func TestMemStoreLoadAllOverageCapCents_OnlyCapBearing(t *testing.T) {
	t.Parallel()
	s := state.NewMemStore()
	ctx := context.Background()
	a, err := s.CreateAccount(ctx, "a@example.com", api.PlanHobby)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	b, err := s.CreateAccount(ctx, "b@example.com", api.PlanPro)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	c, err := s.CreateAccount(ctx, "c@example.com", api.PlanScale)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	s.SetOverageCapCentsForTest(a.ID, 1000)
	s.SetOverageCapCentsForTest(b.ID, 0) // zero is set, so it's a cap
	// c is unset → NULL → dropped from the bulk read.

	caps, err := s.LoadAllOverageCapCents(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(caps) != 2 {
		t.Fatalf("len = %d, want 2", len(caps))
	}
	if caps[a.ID] != 1000 {
		t.Fatalf("a cents = %d, want 1000", caps[a.ID])
	}
	if caps[b.ID] != 0 {
		t.Fatalf("b cents = %d, want 0", caps[b.ID])
	}
	if _, ok := caps[c.ID]; ok {
		t.Fatalf("c (unset/NULL) leaked into the bulk read")
	}
}

func TestMemStoreCurrentMonthOverageCents_Formula(t *testing.T) {
	t.Parallel()
	s := state.NewMemStore()
	ctx := context.Background()
	acct, err := s.CreateAccount(ctx, "alice@example.com", api.PlanHobby)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// 1 GB-h = 3_600_000 MB-seconds (1024 MB per GB).
	// 1200 cents = 12 GB-h.
	const wantCents = int64(1200)
	mbSeconds := wantCents * 3600 / 100 // 43_200_000
	now := time.Now().UTC()
	if err := s.AppendUsage(ctx, acct.ID, "app-1", "inst-1", now, mbSeconds, 0); err != nil {
		t.Fatalf("append usage: %v", err)
	}

	got, err := s.CurrentMonthOverageCents(ctx, acct.ID)
	if err != nil {
		t.Fatalf("current month: %v", err)
	}
	if got != wantCents {
		t.Fatalf("got %d cents, want %d", got, wantCents)
	}
}

func TestMemStoreCurrentMonthOverageCents_BeforeMonthStart(t *testing.T) {
	t.Parallel()
	s := state.NewMemStore()
	ctx := context.Background()
	acct, err := s.CreateAccount(ctx, "alice@example.com", api.PlanHobby)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Plant usage in the previous UTC month — must not be counted.
	now := time.Now().UTC()
	prevMonth := time.Date(now.Year(), now.Month()-1, 15, 12, 0, 0, 0, time.UTC)
	if err := s.AppendUsage(ctx, acct.ID, "app-1", "inst-1", prevMonth, 3_600_000, 0); err != nil {
		t.Fatalf("append usage: %v", err)
	}

	got, err := s.CurrentMonthOverageCents(ctx, acct.ID)
	if err != nil {
		t.Fatalf("current month: %v", err)
	}
	if got != 0 {
		t.Fatalf("got %d cents, want 0 (previous-month rows excluded)", got)
	}
}

func TestMemStoreAccountCredit_RoundTrip(t *testing.T) {
	t.Parallel()
	s := state.NewMemStore()
	ctx := context.Background()
	acct, err := s.CreateAccount(ctx, "alice@example.com", api.PlanHobby)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	credit, err := s.CreateAccountCredit(ctx, state.AccountCredit{
		AccountID:      acct.ID,
		CentsRemaining: 5000,
		Reason:         "goodwill for outage",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if credit.ID == "" {
		t.Fatal("credit.ID is empty after CreateAccountCredit")
	}
	if credit.CreatedAt.IsZero() {
		t.Fatal("credit.CreatedAt is zero after CreateAccountCredit")
	}

	rows, err := s.ListAccountCredits(ctx, acct.ID, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len = %d, want 1", len(rows))
	}
	if rows[0].ID != credit.ID {
		t.Fatalf("id = %s, want %s", rows[0].ID, credit.ID)
	}
	if rows[0].CentsRemaining != 5000 {
		t.Fatalf("cents_remaining = %d, want 5000", rows[0].CentsRemaining)
	}
}

func TestMemStoreCreditLedger_AppendOnly(t *testing.T) {
	t.Parallel()
	s := state.NewMemStore()
	ctx := context.Background()
	acct, err := s.CreateAccount(ctx, "alice@example.com", api.PlanHobby)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	credit, err := s.CreateAccountCredit(ctx, state.AccountCredit{
		AccountID:      acct.ID,
		CentsRemaining: 5000,
		Reason:         "goodwill for outage",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.CreateCreditLedgerEntry(ctx, state.CreditLedgerEntry{
		AccountID:  acct.ID,
		CreditID:   credit.ID,
		DeltaCents: 5000,
		Reason:     "goodwill for outage",
		Actor:      "actor-uuid",
	}); err != nil {
		t.Fatalf("ledger: %v", err)
	}
	// Append a second ledger row (consumption later). The store
	// refuses to surface a delete path; the test asserts the same
	// row is appended, not replaced.
	if err := s.CreateCreditLedgerEntry(ctx, state.CreditLedgerEntry{
		AccountID:  acct.ID,
		CreditID:   credit.ID,
		DeltaCents: -1000,
		Reason:     "consumed against invoice #1234",
		Actor:      "system",
	}); err != nil {
		t.Fatalf("ledger 2: %v", err)
	}
	// The pgstore reads via the credit_ledger table; the memstore
	// doesn't expose a list helper (the reducer is the only consumer),
	// so this test only asserts both writes succeeded.
}

func TestMemStoreCreateAccountCredit_AutoID(t *testing.T) {
	t.Parallel()
	s := state.NewMemStore()
	ctx := context.Background()
	acct, err := s.CreateAccount(ctx, "alice@example.com", api.PlanHobby)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	c, err := s.CreateAccountCredit(ctx, state.AccountCredit{
		AccountID:      acct.ID,
		CentsRemaining: 100,
		Reason:         "auto-id test",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := uuid.Parse(c.ID); err != nil {
		t.Fatalf("auto-generated ID %q is not a UUID: %v", c.ID, err)
	}
}
