package state_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
	"github.com/onebox-faas/faas/pkg/state"
)

// pgStoreInvoicesWithPool returns the store + the underlying pool so a test
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

// TestPgStore_Invoices_UniqueConstraint_Tripwire pins the
// (account_id, provider, provider_invoice_id) unique constraint that
// is the webhook-replay guard. Without it, two concurrent retries
// for the same provider_invoice_id would silently double-insert and
// the customer's invoice list would show duplicates. The reviewer
// (issue #259 review) flagged that PR A lands the constraint but no
// test enforces it; PR B will rely on this tripwire when it wires
// UpsertInvoice via ON CONFLICT DO NOTHING.
func TestPgStore_Invoices_UniqueConstraint_Tripwire(t *testing.T) {
	s, pool, ctx := pgStoreInvoicesWithPool(t)
	acct, err := s.CreateAccount(ctx, "alice@example.com", api.PlanHobby)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	july := time.Date(2026, 7, 31, 23, 59, 59, 0, time.UTC)

	seedInvoiceFixture(t, pool, ctx, acct.ID, "stripe", "in_dup", july, 900)

	// Second insert with the same (account_id, provider,
	// provider_invoice_id) — must error with a unique-violation SQLSTATE.
	// We bypass the store here because PR A has no UpsertInvoice writer
	// (PR B adds it via ON CONFLICT DO NOTHING). The tripwire is on
	// the migration contract, not on the writer.
	id := uuid.NewString()
	periodStart := july.AddDate(0, 0, -30)
	_, err = pool.Exec(ctx,
		`insert into invoices (id, account_id, provider, provider_invoice_id,
		                       number, status, period_start, period_end,
		                       subtotal_cents, tax_cents, total_cents,
		                       amount_paid_cents, currency, pdf_available,
		                       hosted_url, raw)
		 values ($1, $2, 'stripe', 'in_dup', '', 'paid', $3, $4,
		         900, 0, 900, 900, 'eur', true, '', '{}'::jsonb)`,
		id, acct.ID, periodStart, july)
	if err == nil {
		t.Fatalf("unique constraint missing: second insert with same (account_id, provider, provider_invoice_id) succeeded")
	}
	// SQLSTATE 23505 = unique_violation. Any 23505 here is fine —
	// the migration's unique constraint fired.
	if !strings.Contains(err.Error(), "23505") && !strings.Contains(err.Error(), "unique") {
		t.Fatalf("expected unique violation, got: %v", err)
	}

	// Lock the row count: the customer's invoice list must not show
	// duplicates even after the failed retry.
	var rowCount int
	if err := pool.QueryRow(ctx,
		`select count(*) from invoices where account_id = $1`, acct.ID,
	).Scan(&rowCount); err != nil {
		t.Fatalf("count invoices: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("unique violation did not prevent duplicate insert: count=%d", rowCount)
	}
}

// TestPgStore_Invoices_NonNegativeMoney_Tripwire pins the migration's
// CHECK (total_cents >= 0) (and the sibling checks on subtotal/tax/
// amount_paid). The plan defers credit notes to a separate row type
// — invoice rows are positive-bill only. A future PR that loosens
// the CHECK to permit credit-note adjustments on the same row would
// silently double-count at billing time; this tripwire prevents
// that. The Dashboard and CLI formatters (formatCentsEuros /
// renderInvoices) flip the sign on negative cents, so a regression
// here would also surface as €-9.00 in the dashboard.
func TestPgStore_Invoices_NonNegativeMoney_Tripwire(t *testing.T) {
	s, pool, ctx := pgStoreInvoicesWithPool(t)
	acct, err := s.CreateAccount(ctx, "alice@example.com", api.PlanHobby)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	july := time.Date(2026, 7, 31, 23, 59, 59, 0, time.UTC)
	periodStart := july.AddDate(0, 0, -30)

	// Negative total_cents — must be rejected by the CHECK.
	id := uuid.NewString()
	_, err = pool.Exec(ctx,
		`insert into invoices (id, account_id, provider, provider_invoice_id,
		                       number, status, period_start, period_end,
		                       subtotal_cents, tax_cents, total_cents,
		                       amount_paid_cents, currency, pdf_available,
		                       hosted_url, raw)
		 values ($1, $2, 'stripe', 'in_neg', '', 'paid', $3, $4,
		         0, 0, -1, 0, 'eur', true, '', '{}'::jsonb)`,
		id, acct.ID, periodStart, july)
	if err == nil {
		t.Fatalf("CHECK missing: insert with total_cents=-1 succeeded")
	}
	if !strings.Contains(err.Error(), "23514") && !strings.Contains(err.Error(), "check") {
		t.Fatalf("expected check_violation (23514), got: %v", err)
	}

	// Negative subtotal_cents — same CHECK.
	_, err = pool.Exec(ctx,
		`insert into invoices (id, account_id, provider, provider_invoice_id,
		                       number, status, period_start, period_end,
		                       subtotal_cents, tax_cents, total_cents,
		                       amount_paid_cents, currency, pdf_available,
		                       hosted_url, raw)
		 values ($1, $2, 'stripe', 'in_neg_sub', '', 'paid', $3, $4,
		         -1, 0, 0, 0, 'eur', true, '', '{}'::jsonb)`,
		id, acct.ID, periodStart, july)
	if err == nil {
		t.Fatalf("CHECK missing: insert with subtotal_cents=-1 succeeded")
	}

	// Sanity: positive row lands.
	seedInvoiceFixture(t, pool, ctx, acct.ID, "stripe", "in_pos", july, 900)
}

// TestPgStore_ListInvoicesForAccount_MonthBoundary pins the Dec→Jan
// roll-over on the month filter. The handler uses
// `date_trunc('month', $2::timestamptz)` against `period_end` — a
// row at 2026-12-31T23:59:59Z belongs to month=2026-12, a row at
// 2027-01-01T00:00:00Z belongs to month=2027-01. Off-by-one on the
// half-open boundary would silently mis-bucket invoices for any
// customer whose billing cycle crosses the calendar boundary.
func TestPgStore_ListInvoicesForAccount_MonthBoundary(t *testing.T) {
	s, pool, ctx := pgStoreInvoicesWithPool(t)
	acct, err := s.CreateAccount(ctx, "alice@example.com", api.PlanHobby)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	dec31 := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
	jan01 := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	seedInvoiceFixture(t, pool, ctx, acct.ID, "stripe", "in_dec", dec31, 900)
	seedInvoiceFixture(t, pool, ctx, acct.ID, "stripe", "in_jan", jan01, 900)

	// month=2026-12 must return only Dec.
	dec := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	decRows, err := s.ListInvoicesForAccount(ctx, acct.ID, &dec, time.Time{}, 25)
	if err != nil {
		t.Fatalf("ListInvoicesForAccount(dec): %v", err)
	}
	if len(decRows) != 1 || decRows[0].ProviderInvoiceID != "in_dec" {
		t.Fatalf("month=2026-12 returned wrong rows: got %+v want [in_dec]", decRows)
	}

	// month=2027-01 must return only Jan — the half-open boundary
	// must NOT leak Dec 31 into Jan.
	jan := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	janRows, err := s.ListInvoicesForAccount(ctx, acct.ID, &jan, time.Time{}, 25)
	if err != nil {
		t.Fatalf("ListInvoicesForAccount(jan): %v", err)
	}
	if len(janRows) != 1 || janRows[0].ProviderInvoiceID != "in_jan" {
		t.Fatalf("month=2027-01 returned wrong rows: got %+v want [in_jan]", janRows)
	}

	// month=nil (all months) must return both in descending order.
	allRows, err := s.ListInvoicesForAccount(ctx, acct.ID, nil, time.Time{}, 25)
	if err != nil {
		t.Fatalf("ListInvoicesForAccount(all): %v", err)
	}
	if len(allRows) != 2 {
		t.Fatalf("month=nil expected 2 rows, got %d", len(allRows))
	}
	if allRows[0].ProviderInvoiceID != "in_jan" || allRows[1].ProviderInvoiceID != "in_dec" {
		t.Fatalf("ordering broken: got [%s, %s] want [in_jan, in_dec]",
			allRows[0].ProviderInvoiceID, allRows[1].ProviderInvoiceID)
	}
}
