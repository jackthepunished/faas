package state_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
	"github.com/onebox-faas/faas/pkg/state"
)

// pgStoreWithPool returns the store + the underlying pool so a test
// can seed invoice rows directly (PR A does not expose an upsert
// writer yet; PR B wires UpsertInvoice via webhook ingestion). Same
// pattern as pgStoreAccountDeletionWithPool.
func pgStoreInvoicesWithPool(t *testing.T) (*state.PgStore, *pgxpool.Pool, context.Context) {
	t.Helper()
	pool := pgtest.Open(t)
	ctx := context.Background()
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return state.NewPgStore(pool), pool, ctx
}

func seedInvoiceFixture(t *testing.T, pool *pgxpool.Pool, ctx context.Context,
	accountID, provider, providerInvoiceID string,
	periodEnd time.Time, totalCents int64) state.Invoice {
	t.Helper()
	id := uuid.NewString()
	periodStart := periodEnd.AddDate(0, 0, -30)
	_, err := pool.Exec(ctx,
		`insert into invoices (id, account_id, provider, provider_invoice_id, number, status,
		                       period_start, period_end, subtotal_cents, tax_cents,
		                       total_cents, amount_paid_cents, currency, pdf_available,
		                       hosted_url, raw)
		 values ($1, $2, $3, $4, '', 'paid',
		         $5, $6, $7, 0,
		         $7, $7, 'eur', true,
		         '', '{}'::jsonb)`,
		id, accountID, provider, providerInvoiceID, periodStart, periodEnd, totalCents)
	if err != nil {
		t.Fatalf("seed invoice: %v", err)
	}
	return state.Invoice{
		ID:                id,
		AccountID:         accountID,
		Provider:          provider,
		ProviderInvoiceID: providerInvoiceID,
		Status:            "paid",
		PeriodStart:       periodStart,
		PeriodEnd:         periodEnd,
		TotalCents:        totalCents,
		Currency:          "eur",
		PDFAvailable:      true,
	}
}

func TestPgStore_ListInvoicesForAccount_OrderingAndScope(t *testing.T) {
	s, pool, ctx := pgStoreInvoicesWithPool(t)
	acct, err := s.CreateAccount(ctx, "alice@example.com", api.PlanHobby)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	other, err := s.CreateAccount(ctx, "bob@example.com", api.PlanHobby)
	if err != nil {
		t.Fatalf("CreateAccount(bob): %v", err)
	}
	july := time.Date(2026, 7, 31, 23, 59, 59, 0, time.UTC)
	aug := time.Date(2026, 8, 31, 23, 59, 59, 0, time.UTC)
	seedInvoiceFixture(t, pool, ctx, acct.ID, "stripe", "in_old", july, 900)
	seedInvoiceFixture(t, pool, ctx, acct.ID, "stripe", "in_new", aug, 900)
	// Other account — must not appear in alice's listing.
	seedInvoiceFixture(t, pool, ctx, other.ID, "stripe", "in_other", aug, 100)

	rows, err := s.ListInvoicesForAccount(ctx, acct.ID, nil, time.Time{}, 25)
	if err != nil {
		t.Fatalf("ListInvoicesForAccount: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (account scope broken)", len(rows))
	}
	if !rows[0].PeriodEnd.After(rows[1].PeriodEnd) {
		t.Fatalf("ordering broken: first=%v second=%v", rows[0].PeriodEnd, rows[1].PeriodEnd)
	}
}

func TestPgStore_ListInvoicesForAccount_MonthFilter(t *testing.T) {
	s, pool, ctx := pgStoreInvoicesWithPool(t)
	acct, err := s.CreateAccount(ctx, "alice@example.com", api.PlanHobby)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	july := time.Date(2026, 7, 31, 23, 59, 59, 0, time.UTC)
	aug := time.Date(2026, 8, 31, 23, 59, 59, 0, time.UTC)
	seedInvoiceFixture(t, pool, ctx, acct.ID, "stripe", "in_jul", july, 900)
	seedInvoiceFixture(t, pool, ctx, acct.ID, "stripe", "in_aug", aug, 900)

	julyPtr := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	rows, err := s.ListInvoicesForAccount(ctx, acct.ID, &julyPtr, time.Time{}, 25)
	if err != nil {
		t.Fatalf("ListInvoicesForAccount: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("month filter broken: got %d rows, want 1", len(rows))
	}
	if rows[0].ProviderInvoiceID != "in_jul" {
		t.Fatalf("month filter returned %q, want in_jul", rows[0].ProviderInvoiceID)
	}
}

func TestPgStore_ListInvoicesForAccount_EmptyHistory(t *testing.T) {
	s, _, ctx := pgStoreInvoicesWithPool(t)
	acct, err := s.CreateAccount(ctx, "alice@example.com", api.PlanHobby)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	rows, err := s.ListInvoicesForAccount(ctx, acct.ID, nil, time.Time{}, 25)
	if err != nil {
		t.Fatalf("ListInvoicesForAccount: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("got %d rows, want 0", len(rows))
	}
}

func TestPgStore_ListInvoicesForAccount_FKDeleteCascade(t *testing.T) {
	// Hard-delete the account → invoices must go too (GDPR).
	// DeleteAccount is the prod seam; using it directly keeps the
	// test honest about the cascade contract. (Migration 00047 has
	// ON DELETE CASCADE on account_id; this test locks the migration
	// against silent drop.)
	s, pool, ctx := pgStoreInvoicesWithPool(t)
	acct, err := s.CreateAccount(ctx, "alice@example.com", api.PlanHobby)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	july := time.Date(2026, 7, 31, 23, 59, 59, 0, time.UTC)
	seedInvoiceFixture(t, pool, ctx, acct.ID, "stripe", "in_to_cascade", july, 900)

	if err := s.MarkAccountDeletionPending(ctx, acct.ID); err != nil {
		t.Fatalf("MarkAccountDeletionPending: %v", err)
	}
	if err := s.DeleteAccount(ctx, acct.ID); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	var remaining int
	if err := pool.QueryRow(ctx, `select count(*) from invoices where account_id = $1`, acct.ID).Scan(&remaining); err != nil {
		t.Fatalf("count invoices: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("FK cascade broken: %d invoices left after DeleteAccount", remaining)
	}
}
