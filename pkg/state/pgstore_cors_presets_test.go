package state_test

// PR-A of the cors-presets rollout (issue #975 item #4 /
// Mega-Foundation #979-b, slot 00294). Round-trips the read path
// against a real Postgres cluster so a typo in the SELECT column
// order, a missing scan of one of the text[] fields, or an
// app_id NULL handling bug can't ship silently.
//
// Same pgtest.Open skip-when-no-pg pattern as
// pgstore_edge_rules_test.go. PR-A ships only the data model and
// the read methods; the write surface (Create/Update/Delete) and
// the per-rule cors.preset_id field land in PR-B (#979-c, slot
// 00295), so these tests exercise the read path via raw SQL
// INSERTs into the new table — the alternative (waiting for PR-B
// to land and then adding coverage) leaves PR-A without a
// regression net for the round trip.

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onebox-faas/faas/pkg/state"
)

// pgSampleCorsPreset is the seed row used by the read-path
// tests. Fresh per call so callers can mutate fields without
// aliasing. AppID is empty for the account-wide case; a separate
// helper returns the app-scoped variant.
func pgSampleCorsPreset(accountID string) state.CorsPreset {
	return state.CorsPreset{
		AccountID:        accountID,
		AppID:            "",
		Name:             "preset-" + uuid.NewString(),
		Description:      "shared allowlist",
		AllowOrigins:     []string{"https://app.example.com", "https://admin.example.com"},
		AllowMethods:     []string{"GET", "POST"},
		AllowHeaders:     []string{"Authorization", "X-Request-Id"},
		ExposeHeaders:    []string{"X-Request-Id"},
		AllowCredentials: false,
		MaxAgeSeconds:    600,
	}
}

// pgSampleCorsPresetAppScoped is the app-scoped variant. Same
// shape as pgSampleCorsPreset but with AppID set and a different
// name (the (account_id, COALESCE(app_id, '00..00'), name) UNIQUE
// constraint would reject a duplicate name even across scope
// boundaries, see migrations/00294_cors_presets.sql for the
// rationale).
func pgSampleCorsPresetAppScoped(accountID, appID string) state.CorsPreset {
	p := pgSampleCorsPreset(accountID)
	p.AppID = appID
	p.Name = "preset-app-" + uuid.NewString()
	return p
}

// pgInsertCorsPreset bypasses the (not-yet-existing in PR-A) write
// path and inserts a row directly via the SQL the migration
// declares. The column list mirrors corsPresetSelectCols in
// pgstore.go so a future drift surfaces as a Scan error here
// rather than at first customer write.
func pgInsertCorsPreset(t *testing.T, ctx context.Context, pool *pgxpool.Pool, p state.CorsPreset) state.CorsPreset {
	t.Helper()
	if p.ID == "" {
		p.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	if p.UpdatedAt.IsZero() {
		p.UpdatedAt = now
	}
	var appID *string
	if p.AppID != "" {
		appID = &p.AppID
	}
	var desc *string
	if p.Description != "" {
		desc = &p.Description
	}
	_, err := pool.Exec(ctx, `
		insert into cors_presets (
			id, account_id, app_id, name, description,
			allow_origins, allow_methods, allow_headers, expose_headers,
			allow_credentials, max_age_seconds, created_at, updated_at
		) values (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9,
			$10, $11, $12, $13
		)`,
		p.ID, p.AccountID, appID, p.Name, desc,
		p.AllowOrigins, p.AllowMethods, p.AllowHeaders, p.ExposeHeaders,
		p.AllowCredentials, p.MaxAgeSeconds, p.CreatedAt, p.UpdatedAt,
	)
	if err != nil {
		t.Fatalf("insert cors_preset %s: %v", p.Name, err)
	}
	return p
}

// pgInsertCorsPresetRaw is the no-t-fail-t.Helper variant used
// by the uniqueness test that asserts on the underlying error.
func pgInsertCorsPresetRaw(ctx context.Context, pool *pgxpool.Pool, p state.CorsPreset) (string, error) {
	if p.ID == "" {
		p.ID = uuid.NewString()
	}
	var appID *string
	if p.AppID != "" {
		appID = &p.AppID
	}
	var desc *string
	if p.Description != "" {
		desc = &p.Description
	}
	now := time.Now().UTC()
	_, err := pool.Exec(ctx, `
		insert into cors_presets (
			id, account_id, app_id, name, description,
			allow_origins, allow_methods, allow_headers, expose_headers,
			allow_credentials, max_age_seconds, created_at, updated_at
		) values (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9,
			$10, $11, $12, $13
		)`,
		p.ID, p.AccountID, appID, p.Name, desc,
		p.AllowOrigins, p.AllowMethods, p.AllowHeaders, p.ExposeHeaders,
		p.AllowCredentials, p.MaxAgeSeconds, now, now,
	)
	return p.ID, err
}

// TestPgStore_CorsPreset_AccountWideRoundTrip exercises the read
// path for an account-wide preset (app_id IS NULL). Pins the
// scanCorsPresetCols column order against the migration and proves
// the AppID-empty and Description-empty round-trip works.
func TestPgStore_CorsPreset_AccountWideRoundTrip(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	acct, _, _ := seedLiveDeploy(t, s, ctx, "cors-acct-"+strconv.Itoa(int(time.Now().UnixNano())))

	seed := pgSampleCorsPreset(acct)
	seed.Description = "" // exercise the nullable path
	seed.AllowHeaders = nil
	seed.AllowCredentials = true
	seed.MaxAgeSeconds = 3600
	pgInsertCorsPreset(t, ctx, pool, seed)

	got, err := s.GetCorsPresetByID(ctx, seed.ID)
	if err != nil {
		t.Fatalf("GetCorsPresetByID: %v", err)
	}
	if got.AccountID != acct {
		t.Errorf("AccountID = %q, want %q", got.AccountID, acct)
	}
	if got.AppID != "" {
		t.Errorf("AppID = %q, want empty (account-wide)", got.AppID)
	}
	if got.Name != seed.Name {
		t.Errorf("Name = %q, want %q", got.Name, seed.Name)
	}
	if got.Description != "" {
		t.Errorf("Description = %q, want empty", got.Description)
	}
	if !got.AllowCredentials {
		t.Errorf("AllowCredentials = false, want true")
	}
	if got.MaxAgeSeconds != 3600 {
		t.Errorf("MaxAgeSeconds = %d, want 3600", got.MaxAgeSeconds)
	}
	if len(got.AllowOrigins) != 2 {
		t.Errorf("AllowOrigins len = %d, want 2", len(got.AllowOrigins))
	}
	if len(got.AllowMethods) != 2 {
		t.Errorf("AllowMethods len = %d, want 2", len(got.AllowMethods))
	}
	// AllowHeaders was nil on insert but the column has DEFAULT '{}'
	// so the round-trip should reflect that (an empty array, not
	// nil). The compile-side merge treats both as "no header
	// allowlist" so this is a no-op for the customer but a
	// non-empty allowlist cannot be nil in the row shape.
	if got.AllowHeaders == nil {
		t.Errorf("AllowHeaders = nil, want empty array (DB default)")
	}
}

// TestPgStore_CorsPreset_AppScopedRoundTrip exercises the read
// path for an app-scoped preset (app_id NOT NULL). Pins the
// nullable-app_id branch in scanCorsPresetCols.
func TestPgStore_CorsPreset_AppScopedRoundTrip(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	acct, app, _ := seedLiveDeploy(t, s, ctx, "cors-app-"+strconv.Itoa(int(time.Now().UnixNano())))

	seed := pgSampleCorsPresetAppScoped(acct, app)
	pgInsertCorsPreset(t, ctx, pool, seed)

	got, err := s.GetCorsPresetByID(ctx, seed.ID)
	if err != nil {
		t.Fatalf("GetCorsPresetByID: %v", err)
	}
	if got.AppID != app {
		t.Errorf("AppID = %q, want %q", got.AppID, app)
	}
	if got.Description != "shared allowlist" {
		t.Errorf("Description = %q, want %q", got.Description, "shared allowlist")
	}
	if len(got.ExposeHeaders) != 1 || got.ExposeHeaders[0] != "X-Request-Id" {
		t.Errorf("ExposeHeaders = %v, want [X-Request-Id]", got.ExposeHeaders)
	}
}

// TestPgStore_CorsPreset_GetByID_UnknownReturnsErrNotFound pins
// the ErrNotFound mapping in scanCorsPreset so a deleted preset
// surfaces as 422 at the apid boundary, not a 500.
func TestPgStore_CorsPreset_GetByID_UnknownReturnsErrNotFound(t *testing.T) {
	s, _, ctx := pgStoreWithPool(t)
	_, err := s.GetCorsPresetByID(ctx, uuid.NewString())
	if !errors.Is(err, state.ErrNotFound) {
		t.Errorf("GetCorsPresetByID(unknown) = %v, want ErrNotFound", err)
	}
}

// TestPgStore_CorsPreset_ListForAccount_OrdersByAppIDNullsFirst
// pins the deterministic ordering required by the gatewayd
// compile-side cache key. account-wide rows must come before
// app-scoped rows; within each group, alphabetical by name.
func TestPgStore_CorsPreset_ListForAccount_OrdersByAppIDNullsFirst(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	acct, app, _ := seedLiveDeploy(t, s, ctx, "cors-order-"+strconv.Itoa(int(time.Now().UnixNano())))

	// Insert in deliberately scrambled order to prove the SELECT
	// does the sorting, not the INSERT order.
	pgInsertCorsPreset(t, ctx, pool, pgSampleCorsPresetAppScoped(acct, app))
	pgInsertCorsPreset(t, ctx, pool, pgSampleCorsPreset(acct))
	pgInsertCorsPreset(t, ctx, pool, pgSampleCorsPresetAppScoped(acct, app))

	got, err := s.ListCorsPresetsForAccount(ctx, acct)
	if err != nil {
		t.Fatalf("ListCorsPresetsForAccount: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	// account-wide (AppID == "") comes first.
	if got[0].AppID != "" {
		t.Errorf("got[0].AppID = %q, want empty (account-wide first)", got[0].AppID)
	}
	// remaining two are app-scoped; their names must be sorted.
	if got[1].AppID != app || got[2].AppID != app {
		t.Errorf("got[1].AppID=%q got[2].AppID=%q, want both = %q", got[1].AppID, got[2].AppID, app)
	}
	if got[1].Name > got[2].Name {
		t.Errorf("app-scoped rows not sorted by name: %q > %q", got[1].Name, got[2].Name)
	}
}

// TestPgStore_CorsPreset_ListForAccount_ScopesByAccount pins the
// cross-account guard. A preset created under account A must not
// appear in account B's listing — IDOR would expose one tenant's
// preset names to another tenant's UI.
func TestPgStore_CorsPreset_ListForAccount_ScopesByAccount(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	acctA, _, _ := seedLiveDeploy(t, s, ctx, "cors-idorA-"+strconv.Itoa(int(time.Now().UnixNano())))
	acctB, _, _ := seedLiveDeploy(t, s, ctx, "cors-idorB-"+strconv.Itoa(int(time.Now().UnixNano())))

	pgInsertCorsPreset(t, ctx, pool, pgSampleCorsPreset(acctA))
	pgInsertCorsPreset(t, ctx, pool, pgSampleCorsPreset(acctB))

	listA, err := s.ListCorsPresetsForAccount(ctx, acctA)
	if err != nil {
		t.Fatalf("ListCorsPresetsForAccount(A): %v", err)
	}
	if len(listA) != 1 {
		t.Errorf("account A listing len = %d, want 1", len(listA))
	}
	if len(listA) > 0 && listA[0].AccountID != acctA {
		t.Errorf("account A leaked: got AccountID %q", listA[0].AccountID)
	}
}

// TestPgStore_CorsPreset_ListForApp_ExcludesAccountWide pins
// the complement of the above: a ListCorsPresetsForApp call must
// NOT return the account-wide preset the same account owns. The
// compile path unions both lists, so the per-app method is
// strict-scope.
func TestPgStore_CorsPreset_ListForApp_ExcludesAccountWide(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	acct, app, _ := seedLiveDeploy(t, s, ctx, "cors-scope-"+strconv.Itoa(int(time.Now().UnixNano())))

	pgInsertCorsPreset(t, ctx, pool, pgSampleCorsPreset(acct))               // account-wide
	pgInsertCorsPreset(t, ctx, pool, pgSampleCorsPresetAppScoped(acct, app)) // app-scoped

	list, err := s.ListCorsPresetsForApp(ctx, app)
	if err != nil {
		t.Fatalf("ListCorsPresetsForApp: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("len = %d, want 1 (only the app-scoped row)", len(list))
	}
	if list[0].AppID != app {
		t.Errorf("AppID = %q, want %q", list[0].AppID, app)
	}
}

// TestPgStore_CorsPreset_UniqueNameSameAccountBothScopes pins
// the (account_id, COALESCE(app_id, '00..00'), name) UNIQUE
// constraint. Two presets with the same name — one account-wide,
// one app-scoped — must both insert; a duplicate name within
// the same scope must reject.
func TestPgStore_CorsPreset_UniqueNameSameAccountBothScopes(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	acct, app, _ := seedLiveDeploy(t, s, ctx, "cors-uniq-"+strconv.Itoa(int(time.Now().UnixNano())))

	sharedName := "shared-name-" + uuid.NewString()
	wide := pgSampleCorsPreset(acct)
	wide.Name = sharedName
	scoped := pgSampleCorsPresetAppScoped(acct, app)
	scoped.Name = sharedName

	pgInsertCorsPreset(t, ctx, pool, wide)
	pgInsertCorsPreset(t, ctx, pool, scoped) // different COALESCE bucket — must succeed

	// duplicate within the same bucket must reject with 23505.
	dup := pgSampleCorsPresetAppScoped(acct, app)
	dup.Name = sharedName
	if _, err := pgInsertCorsPresetRaw(ctx, pool, dup); err == nil {
		t.Errorf("duplicate name within (acct, app) bucket was accepted; want 23505")
	}
}
