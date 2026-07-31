//go:build !no_pg

package migrations_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00068_BuilderUsage(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	defer pool.Close()
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}

	// genRandom returns a fresh UUID for column-default use in the
	// NOT NULL test below.
	genRandom := func(_ *testing.T) uuid.UUID { return uuid.New() }

	// Column existence + types + defaults. 00068 uses
	// search_path-relative identifiers (no `public.` prefix), so the
	// column lookup queries `current_schema()` rather than a
	// hardcoded schema name — the table lives in `public` for prod
	// connections and in `faas_test_<hex>` for pgtest-isolated test
	// schemas. Schema scoping = searches current_schema() only. This
	// is the fix that closes the 40P01 deadlock on pg_class when N
	// parallel test packages each try CREATE TABLE public.builder_usage
	// against the same cluster (CI run 30645758787,
	// TestPg_ClaimCliAuthCode_BindsAccountID).
	want := map[string]string{
		"build_id":    "uuid",
		"account_id":  "uuid",
		"app_id":      "uuid",
		"finished_at": "timestamp with time zone",
		"kind":        "text",
		"seconds":     "bigint",
	}
	for col, typ := range want {
		var got string
		err := pool.QueryRow(ctx, `
			SELECT data_type FROM information_schema.columns
			 WHERE table_schema = current_schema()
			   AND table_name = 'builder_usage' AND column_name = $1`, col).Scan(&got)
		if err != nil {
			t.Fatalf("column %s lookup: %v", col, err)
		}
		if got != typ {
			t.Errorf("builder_usage.%s data_type = %q, want %q", col, got, typ)
		}
	}

	// First-write-wins: insert a row, then a redelivered webhook with
	// different seconds → the second is ignored (ON CONFLICT DO
	// NOTHING, enforced by AppendBuilderUsage SQL).
	acctID := uuid.New()
	appID := uuid.New()
	buildID := uuid.New()
	row1 := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
		INSERT INTO builder_usage (build_id, account_id, app_id, finished_at, kind, seconds)
		VALUES ($1, $2, $3, $4, 'railpack', 90)`, buildID, acctID, appID, row1); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	row2 := row1.Add(time.Minute)
	if _, err := pool.Exec(ctx, `
		INSERT INTO builder_usage (build_id, account_id, app_id, finished_at, kind, seconds)
		VALUES ($1, $2, $3, $4, 'dockerfile', 120)
		ON CONFLICT (build_id) DO NOTHING`, buildID, acctID, appID, row2); err != nil {
		t.Fatalf("redelivered insert: %v", err)
	}
	var seconds int64
	var kind string
	if err := pool.QueryRow(ctx, `
		SELECT seconds, kind FROM builder_usage WHERE build_id = $1`,
		buildID).Scan(&seconds, &kind); err != nil {
		t.Fatalf("readback: %v", err)
	}
	if seconds != 90 {
		t.Errorf("seconds = %d, want 90 (first write wins)", seconds)
	}
	if kind != "railpack" {
		t.Errorf("kind = %q, want railpack (first write wins)", kind)
	}

	// NOT NULL on account_id / app_id / finished_at / seconds.
	// Each iteration inserts NULL into the named column. Pre-existing
	// PR-A test bug — the original loop substituted `col` into the
	// column-name list but never NULLed out the corresponding value
	// slot, so the assertions silently passed. Fix here so the
	// tripwire actually fires.
	cases := []struct {
		col       string
		colList   string
		valList   string
		paramVals []any
	}{
		{"account_id", "build_id, account_id, app_id, finished_at, kind, seconds",
			"$1, NULL, $2, $3, 'none', 0",
			[]any{genRandom(t), genRandom(t), time.Now().UTC()}},
		{"app_id", "build_id, account_id, app_id, finished_at, kind, seconds",
			"$1, $2, NULL, $3, 'none', 0",
			[]any{genRandom(t), genRandom(t), time.Now().UTC()}},
		{"finished_at", "build_id, account_id, app_id, finished_at, kind, seconds",
			"$1, $2, $3, NULL, 'none', 0",
			[]any{genRandom(t), genRandom(t), genRandom(t)}},
		{"seconds", "build_id, account_id, app_id, finished_at, kind, seconds",
			"$1, $2, $3, now(), 'none', NULL",
			[]any{genRandom(t), genRandom(t), genRandom(t)}},
	}
	for _, tc := range cases {
		_, err := pool.Exec(ctx,
			"INSERT INTO builder_usage ("+tc.colList+") VALUES ("+tc.valList+")",
			tc.paramVals...)
		if err == nil {
			t.Errorf("expected NOT NULL violation on %s NULL insert", tc.col)
		}
	}

	// Index exists. Scoped to current_schema() because 00068 uses
	// search_path-relative identifiers (the table lives in
	// production's `public` schema and in pgtest's `faas_test_<hex>`
	// schema — pgtest always lands in current_schema()=front-of-search_path).
	var idxdef string
	if err := pool.QueryRow(ctx, `
		SELECT indexdef FROM pg_indexes
		 WHERE schemaname = current_schema()
		   AND tablename = 'builder_usage'
		   AND indexname = 'builder_usage_account_finished_idx'`).Scan(&idxdef); err != nil {
		t.Fatalf("index lookup: %v", err)
	}
	if idxdef == "" {
		t.Fatalf("builder_usage_account_finished_idx missing")
	}
}
