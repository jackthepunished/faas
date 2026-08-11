// pgstore_rekey_null_kid_test.go — PR #825 review fix.
//
// Pins the COALESCE(kid, '') shape in ListAppSecretsForRekey
// (pkg/state/pgstore.go:10072,10097). Pre-PR-A app_secrets rows
// have NULL kid (the column was added by migration 00191 and
// existing rows were left NULL until a PUT rewrites them via
// sealAndPersist). pgx scan of NULL text into a Go string fails
// with "converting NULL to string is unsupported"; without the
// COALESCE, the rekey walk would crash on the first such row
// and never seal a single secret.
//
// This test seeds a row directly with kid=NULL via SQL (the Go
// store API doesn't accept nil kid), then runs
// ListAppSecretsForRekey and asserts no error + empty Kid string.
// Build tag: pg (parallel to pgstore_coverage3_test.go).
//
//go:build pg

package state_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// pgCoverageFixtureWithPool is a local sibling of pgCoverageFixture
// that also returns the underlying pgxpool.Pool. The pre-existing
// fixture (pgstore_coverage_parity_test.go:16) doesn't expose the
// pool, but this test needs raw SQL access to set kid=NULL directly
// — the Go store API never writes NULL.
func pgCoverageFixtureWithPool(t *testing.T) (*state.PgStore, *pgxpool.Pool, context.Context, state.Account, state.App, state.Deployment) {
	t.Helper()
	s, pool, ctx := pgStoreWithPool(t)
	account, err := s.CreateAccount(ctx, "pg-rekey-null-kid-"+uuid.NewString()+"@example.com", api.PlanPro)
	if err != nil {
		t.Fatal(err)
	}
	app, err := s.CreateApp(ctx, state.App{
		AccountID: account.ID, Slug: "pg-rekey-null-kid-" + uuid.NewString(),
		Type: state.AppTypeApp, RAMMB: 512, MaxConcurrency: 5, IdleTimeoutS: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, pool, ctx, account, app, state.Deployment{}
}

// TestPg_ListAppSecretsForRekey_NullKid_DoesNotCrash pins the
// PR #825 fix for pkg/state/pgstore.go:10072,10097. Pre-PR-A
// app_secrets rows can carry NULL kid; the COALESCE in both
// branches of ListAppSecretsForRekey keeps the scan safe.
//
// The empty-cursor branch (no WHERE clause) is the realistic
// path on a fresh PG; the non-empty-cursor branch is the
// restart-resume path. Both must COALESCE.
func TestPg_ListAppSecretsForRekey_NullKid_DoesNotCrash(t *testing.T) {
	s, pool, ctx, account, app, _ := pgCoverageFixtureWithPool(t)

	// Seed a row via the legacy (kid-less) UpsertAppSecret — the
	// pgstore implementation derives kid=NULL for this path
	// (PR-A did not retro-fit UpsertAppSecret to stamp kid; the
	// new sealAndPersist on the PUT path does, but anything
	// written before PR-A merge is NULL).
	if err := s.UpsertAppSecret(ctx, account.ID, app.ID, "DB_URL", []byte("cipher-a")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Belt-and-suspenders: also write directly with NULL kid via
	// SQL to mimic the pre-PR-A row shape byte-for-byte. The
	// legacy UpsertAppSecret path on pgstore writes kid=''
	// (empty, not NULL — see pgstore.go:10376) but the COALESCE
	// change must guard against both shapes, since older
	// pre-migration dumps or manual INSERTs can land NULL.
	if _, err := pool.Exec(ctx,
		`update app_secrets set kid = NULL where app_id = $1 and key = 'DB_URL'`,
		app.ID); err != nil {
		t.Fatalf("force null kid: %v", err)
	}

	// Empty-cursor branch — must NOT error.
	rows, err := s.ListAppSecretsForRekey(ctx, 10, "")
	if err != nil {
		t.Fatalf("empty-cursor branch: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("empty-cursor branch: want 1 row, got %d", len(rows))
	}
	if rows[0].Kid != "" {
		t.Errorf("empty-cursor branch: Kid = %q, want \"\" (NULL coerced via COALESCE)", rows[0].Kid)
	}

	// Non-empty-cursor branch — must NOT error. The cursor format
	// is "<account_id>|<app_id>|<key>" per pgstore.go:10091.
	cursor := account.ID.String() + "|" + app.ID.String() + "|DB_URL"
	rows, err = s.ListAppSecretsForRekey(ctx, 10, cursor)
	if err != nil {
		t.Fatalf("non-empty-cursor branch: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("non-empty-cursor branch: want 1 row, got %d", len(rows))
	}
	if rows[0].Kid != "" {
		t.Errorf("non-empty-cursor branch: Kid = %q, want \"\"", rows[0].Kid)
	}

	// Belt-and-suspenders #2: confirm a kid-stamped row still
	// surfaces its actual kid through the same scan. Otherwise
	// the COALESCE could mask a regression where ALL kids come
	// back empty.
	if err := s.UpsertAppSecretWithKid(ctx, account.ID, app.ID, "API_KEY", "age1abc", []byte("cipher-b")); err != nil {
		t.Fatalf("seed stamped: %v", err)
	}
	rows, err = s.ListAppSecretsForRekey(ctx, 10, "")
	if err != nil {
		t.Fatalf("mixed-kid scan: %v", err)
	}
	stamped := 0
	for _, r := range rows {
		if r.Key == "API_KEY" && r.Kid == "age1abc" {
			stamped++
		}
	}
	if stamped != 1 {
		t.Errorf("mixed-kid scan: stamped row not surfaced verbatim (rows=%v)", rows)
	}

	// Silence unused-import warnings.
	_ = state.ErrNotFound
}
