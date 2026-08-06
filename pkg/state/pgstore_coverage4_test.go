package state_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/onebox-faas/faas/pkg/state"
)

// Fourth batch of PgStore coverage round-trips: node-key upsert,
// compute-node active/delete, invoice read, registry credential quota
// check, and the paddle overage claim reap. All run against a fresh
// migrated schema via pgStore(t).

func TestPg_CoverageNodeKeysAndComputeNode(t *testing.T) {
	s, ctx, _, _, _ := pgCoverageFixture(t)
	nodeID := resolveDefaultLocal(t, ctx, s)
	// UpsertNodeKey — insert + idempotent re-insert.
	keyID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pem := "-----BEGIN PUBLIC KEY-----\nMIIB\n-----END PUBLIC KEY-----\n"
	if err := s.UpsertNodeKey(ctx, nodeID, keyID, pem); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertNodeKey(ctx, nodeID, keyID, pem); err != nil {
		t.Fatal("re-insert node key should be a no-op")
	}
	// SetComputeNodeActive — hit + miss.
	if err := s.SetComputeNodeActive(ctx, nodeID, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetComputeNodeActive(ctx, uuid.NewString(), true); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("set active missing = %v", err)
	}
	// DeleteComputeNode — hit + miss (use a fresh node, not default-local).
	node, err := s.CreateComputeNode(ctx, state.ComputeNode{Name: "pg-del-" + uuid.NewString(), TargetURL: "unix:///run/faas/vmmd.sock", Active: true, AdmissionCeilingMB: 4096, VPCPUs: 4, VCPUBudget: 160, MemMB: 8192, MaxConcurrency: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteComputeNode(ctx, node.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteComputeNode(ctx, node.ID); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("delete node repeat = %v", err)
	}
}

func TestPg_CoverageInvoiceRead(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	account, err := s.CreateAccount(ctx, "pg-inv-"+uuid.NewString()+"@example.com", "pro")
	if err != nil {
		t.Fatal(err)
	}
	// Insert an invoice row directly (there is no store writer for
	// invoices — the webhook path inserts via SQL).
	invID := uuid.NewString()
	_, err = pool.Exec(ctx, `
		insert into invoices (id, account_id, provider, provider_invoice_id, number, status,
		                      period_start, period_end, subtotal_cents, tax_cents,
		                      total_cents, amount_paid_cents, currency, pdf_available)
		values ($1, $2, 'stripe', 'in_123', 'INV-1', 'paid',
		        now(), now(), 1000, 0, 1000, 1000, 'eur', false)`,
		invID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	// GetInvoiceByID — hit + miss.
	if got, err := s.GetInvoiceByID(ctx, invID); err != nil || got.ID != invID || got.TotalCents != 1000 {
		t.Fatalf("invoice = %+v, %v", got, err)
	}
	if _, err := s.GetInvoiceByID(ctx, uuid.NewString()); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("invoice missing = %v", err)
	}
}

func TestPg_CoverageRegistryQuotaCheck(t *testing.T) {
	s, ctx, account, app, _ := pgCoverageFixture(t)
	// UpsertAppRegistryCredential + RegistryCredentialQuotaCheck.
	if err := s.UpsertAppRegistryCredential(ctx, account.ID, app.ID, "registry.example.com", "robot", []byte("sealed")); err != nil {
		t.Fatal(err)
	}
	if n, exists, err := s.RegistryCredentialQuotaCheck(ctx, account.ID, app.ID, "registry.example.com"); err != nil || n != 1 || !exists {
		t.Fatalf("quota check existing = %d/%v, %v", n, exists, err)
	}
	if n, exists, err := s.RegistryCredentialQuotaCheck(ctx, account.ID, app.ID, "other.example.com"); err != nil || n != 1 || exists {
		t.Fatalf("quota check new = %d/%v, %v", n, exists, err)
	}
}

func TestPg_CoveragePaddleReap(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	account, err := s.CreateAccount(ctx, "pg-paddle-"+uuid.NewString()+"@example.com", "pro")
	if err != nil {
		t.Fatal(err)
	}
	window := time.Now().UTC().Truncate(time.Hour)
	// Claim a window. A fresh claim (claimed_at = now()) is within any
	// sane lease, so a 1-minute reap finds nothing stale.
	claimed, err := s.ClaimPaddleOverageWindow(ctx, account.ID, window, "pod-1", time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	if n, err := s.ReapStalePaddleOverageClaims(ctx, time.Minute); err != nil || n != 0 {
		t.Fatalf("reap fresh = %d, %v", n, err)
	}
	// Backdate claimed_at so the claim is stale, then reap it.
	if _, err := pool.Exec(ctx,
		`update paddle_overage_dedupe set claimed_at = now() - interval '1 hour' where account_id = $1 and window_start = $2`,
		account.ID, window); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ReapStalePaddleOverageClaims(ctx, time.Minute); err != nil || n != 1 {
		t.Fatalf("reap stale = %d, %v", n, err)
	}
	// Complete the window after re-claim.
	claimed, err = s.ClaimPaddleOverageWindow(ctx, account.ID, window, "pod-1", time.Minute)
	if err != nil || !claimed {
		t.Fatalf("re-claim = %v, %v", claimed, err)
	}
	if err := s.CompletePaddleOverageWindow(ctx, account.ID, window, 100); err != nil {
		t.Fatal(err)
	}
}
