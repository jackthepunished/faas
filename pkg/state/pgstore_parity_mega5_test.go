// pgstore_parity_mega5_test.go — Coverage Mega-PR #5 cluster 8b:
// pin 3 PgStore methods at 100% on the live Postgres schema via pgtest.
// Each test stands up a fresh schema (pgtest.Open) and exercises the
// real SQL path the unit-tests-pure-* lanes can't reach.
//
// Targets (all 0% before this PR):
//
//   - (s *PgStore) DeploymentSidecarRAMs
//     pkg/state/deployment_sidecar_rams.go:67
//     SQL: SELECT sidecars::text FROM deployments WHERE id = $1
//
//   - (s *PgStore) AuthenticateOIDCBearer
//     pkg/state/pgstore.go:287
//     SQL: oidc_exchanged_tokens hash lookup + accounts by id
//
//   - (s *PgStore) ListDistinctUpstreamHostHashes
//     pkg/state/pgstore.go:18908
//     SQL: GROUP BY (host_redacted_hash, kind, port, host)
//
// Blackbox `package state_test`. Auto-skips on CI lanes without
// Postgres reachable via pgtest.Open's t.Skipf path.

package state_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// --- DeploymentSidecarRAMs (PgStore) -----------------------------

func TestPgStore_DeploymentSidecarRAMs_Happy_Mega5(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	appID := mustCreateAppID(t, s, ctx, "dsr-h")

	depID := insertDeploymentWithSidecars(t, ctx, pool, appID,
		`[{"ram_mb":64},{"ram_mb":128}]`)

	got, err := s.DeploymentSidecarRAMs(ctx, depID)
	if err != nil {
		t.Fatalf("DeploymentSidecarRAMs: %v", err)
	}
	if len(got) != 2 || got[0] != 64 || got[1] != 128 {
		t.Errorf("got = %v, want [64 128]", got)
	}
}

func TestPgStore_DeploymentSidecarRAMs_EmptyArray_Mega5(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	appID := mustCreateAppID(t, s, ctx, "dsr-e")

	// '[]'::jsonb is the column default (migration 00118). The
	// SQL branch in DeploymentSidecarRAMs reads this as the
	// JSON string "[]" (len > 0, not "null"), which the decoder
	// turns into a 0-row []sidecarRAMShape — so the function
	// returns an empty []int, not nil. Pin that contract.
	depID := insertDeploymentWithSidecars(t, ctx, pool, appID, "[]")

	got, err := s.DeploymentSidecarRAMs(ctx, depID)
	if err != nil {
		t.Fatalf("DeploymentSidecarRAMs (empty array): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got = %v, want empty (0-row sidecars jsonb)", got)
	}
}

func TestPgStore_DeploymentSidecarRAMs_UnknownDeployment_Mega5(t *testing.T) {
	s, _, ctx := pgStoreWithPool(t)
	_, err := s.DeploymentSidecarRAMs(ctx, uuid.NewString())
	if err == nil {
		t.Fatal("err = nil, want non-nil (no row)")
	}
}

func TestPgStore_DeploymentSidecarRAMs_EmptyID_Mega5(t *testing.T) {
	s, _, ctx := pgStoreWithPool(t)
	_, err := s.DeploymentSidecarRAMs(ctx, "")
	if err == nil {
		t.Fatal("err = nil, want non-nil (empty deployment_id)")
	}
}

// --- AuthenticateOIDCBearer (PgStore) ----------------------------

func TestPgStore_AuthenticateOIDCBearer_Happy_Mega5(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	acct, err := s.CreateAccount(ctx,
		"oidc-happy-"+uuid.NewString()+"@example.com", api.PlanPro)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	// Seed via raw SQL — InsertOIDCExchangedToken (pgstore.go:444)
	// omits `id` from its INSERT and the table has no DEFAULT, so
	// the production write path fails with 23502. Pin the read
	// path here; the write bug is out of scope for this PR
	// (zero-source-change rule).
	acctUUID, err := uuid.Parse(acct.ID)
	if err != nil {
		t.Fatalf("parse acct.ID: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO oidc_exchanged_tokens
		(id, account_id, token_hash, expires_at, issuer_url, subject, audience, jti)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULL)`,
		uuid.New(), acctUUID, []byte("hash-happy"),
		time.Now().Add(time.Hour),
		"https://issuer.example", "user-1", []string{}); err != nil {
		t.Fatalf("seed oidc_exchanged_tokens: %v", err)
	}
	gotAcct, gotKey, err := s.AuthenticateOIDCBearer(ctx, []byte("hash-happy"))
	if err != nil {
		t.Fatalf("AuthenticateOIDCBearer: %v", err)
	}
	if gotAcct.ID != acct.ID {
		t.Errorf("acct.ID = %q, want %q", gotAcct.ID, acct.ID)
	}
	if gotKey.AccountID != acct.ID {
		t.Errorf("key.AccountID = %q, want %q", gotKey.AccountID, acct.ID)
	}
}

func TestPgStore_AuthenticateOIDCBearer_NotFound_Mega5(t *testing.T) {
	s, _, ctx := pgStoreWithPool(t)
	if _, _, err := s.AuthenticateOIDCBearer(ctx, []byte("never-inserted")); err == nil {
		t.Fatal("err = nil, want non-nil (no row matches hash)")
	}
}

// --- ListDistinctUpstreamHostHashes (PgStore) --------------------

func TestPgStore_ListDistinctUpstreamHostHashes_Empty_Mega5(t *testing.T) {
	s, _, ctx := pgStoreWithPool(t)
	got, err := s.ListDistinctUpstreamHostHashes(ctx)
	if err != nil {
		t.Fatalf("ListDistinctUpstreamHostHashes: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got = %v, want nil (no data_upstreams rows)", got)
	}
}

func TestPgStore_ListDistinctUpstreamHostHashes_GroupBy_Mega5(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	acctID, appID := mustCreateAccountAndAppID(t, s, ctx, "upstr-grp")

	// Two rows with the same (host_redacted_hash, kind, port) — the
	// GROUP BY must collapse them to a single DataUpstreamTarget.
	// data_upstreams requires account_id + source ('inferred'|'explicit');
	// we use 'inferred' to mirror the classifier path. The UNIQUE
	// dedupe key is (app_id, scope, kind, host, port), so the 2 rows
	// must differ on scope to INSERT successfully while still
	// collapsing on the GROUP BY (which doesn't include scope).
	hashA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" // 64 hex
	hostA := fmt.Sprintf("a-%s.example", uuid.NewString()[:8])
	if _, err := pool.Exec(ctx,
		`INSERT INTO data_upstreams(id, account_id, app_id, source, scope, kind, host, port, host_redacted_hash)
		 VALUES ($1, $2, $3, 'inferred', 'prod', 'postgres', $4, 5432, $5)`,
		uuid.New(), acctID, appID, hostA, hashA); err != nil {
		t.Fatalf("seed upstreams #1: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO data_upstreams(id, account_id, app_id, source, scope, kind, host, port, host_redacted_hash)
		 VALUES ($1, $2, $3, 'inferred', 'staging', 'postgres', $4, 5432, $5)`,
		uuid.New(), acctID, appID, hostA, hashA); err != nil {
		t.Fatalf("seed upstreams #2: %v", err)
	}
	// Different port → separate group.
	hostB := fmt.Sprintf("b-%s.example", uuid.NewString()[:8])
	hashB := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := pool.Exec(ctx,
		`INSERT INTO data_upstreams(id, account_id, app_id, source, scope, kind, host, port, host_redacted_hash)
		 VALUES ($1, $2, $3, 'inferred', 'prod', 'postgres', $4, 5433, $5)`,
		uuid.New(), acctID, appID, hostB, hashB); err != nil {
		t.Fatalf("seed upstreams #3: %v", err)
	}

	got, err := s.ListDistinctUpstreamHostHashes(ctx)
	if err != nil {
		t.Fatalf("ListDistinctUpstreamHostHashes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (group by hash+kind+port+host)", len(got))
	}
	seenA, seenB := false, false
	for _, target := range got {
		if target.HostRedactedHash == hashA && target.Port == 5432 {
			seenA = true
		}
		if target.HostRedactedHash == hashB && target.Port == 5433 {
			seenB = true
		}
	}
	if !seenA || !seenB {
		t.Errorf("missing groups: seenA=%v seenB=%v (got=%+v)", seenA, seenB, got)
	}
}

// --- helpers ----------------------------------------------------

// mustCreateAppID stands up a unique account + app and returns the
// app's UUID. Bypasses the heavier CreateDeployment path (which
// holds a FOR UPDATE lock on apps + supersedes prior live/pending
// rows); we only need the app row to satisfy the deployments FK
// for the parity tests below.
func mustCreateAppID(t *testing.T, s *state.PgStore, ctx context.Context, tag string) uuid.UUID {
	t.Helper()
	_, appUUID := mustCreateAccountAndAppID(t, s, ctx, tag)
	return appUUID
}

// mustCreateAccountAndAppID is the underlying helper used by both
// the DeploymentSidecarRAMs tests (need app_id) and the
// ListDistinctUpstreamHostHashes tests (need both account_id and
// app_id).
func mustCreateAccountAndAppID(t *testing.T, s *state.PgStore, ctx context.Context, tag string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	acct, err := s.CreateAccount(ctx,
		"acc-"+tag+"-"+uuid.NewString()+"@example.com", api.PlanPro)
	if err != nil {
		t.Fatalf("CreateAccount(%s): %v", tag, err)
	}
	app, err := s.CreateApp(ctx, state.App{
		AccountID:      acct.ID,
		Slug:           tag + "-" + uuid.NewString(),
		Type:           state.AppTypeApp,
		RAMMB:          256,
		MaxConcurrency: 1,
		IdleTimeoutS:   30,
	})
	if err != nil {
		t.Fatalf("CreateApp(%s): %v", tag, err)
	}
	acctUUID, err := uuid.Parse(acct.ID)
	if err != nil {
		t.Fatalf("acct.ID %q is not a UUID: %v", acct.ID, err)
	}
	appUUID, err := uuid.Parse(app.ID)
	if err != nil {
		t.Fatalf("app.ID %q is not a UUID: %v", app.ID, err)
	}
	return acctUUID, appUUID
}

// insertDeploymentWithSidecars inserts a bare-minimum deployment
// row with the given sidecars JSON literal. Bypasses CreateDeployment
// for the same reason mustCreateAppID does (no FOR UPDATE, no
// supersede — we just need the row to pin DeploymentSidecarRAMs'
// SQL surface).
func insertDeploymentWithSidecars(t *testing.T, ctx context.Context, pool *pgxpool.Pool, appID uuid.UUID, sidecarsJSON string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO deployments(id, app_id, image_digest, status, sidecars) VALUES ($1, $2, $3, $4, $5::jsonb)`,
		id, appID, "sha256:deadbeef", "live", sidecarsJSON); err != nil {
		t.Fatalf("insertDeploymentWithSidecars: %v", err)
	}
	return id
}
