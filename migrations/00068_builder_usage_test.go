//go:build !no_pg

package migrations_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00068_BuilderUsage(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	defer pool.Close()

	// Column existence + types + defaults.
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
		INSERT INTO public.builder_usage (build_id, account_id, app_id, finished_at, kind, seconds)
		VALUES ($1, $2, $3, $4, 'railpack', 90)`, buildID, acctID, appID, row1); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	row2 := row1.Add(time.Minute)
	if _, err := pool.Exec(ctx, `
		INSERT INTO public.builder_usage (build_id, account_id, app_id, finished_at, kind, seconds)
		VALUES ($1, $2, $3, $4, 'dockerfile', 120)
		ON CONFLICT (build_id) DO NOTHING`, buildID, acctID, appID, row2); err != nil {
		t.Fatalf("redelivered insert: %v", err)
	}
	var seconds int64
	var kind string
	if err := pool.QueryRow(ctx, `
		SELECT seconds, kind FROM public.builder_usage WHERE build_id = $1`,
		buildID).Scan(&seconds, &kind); err != nil {
		t.Fatalf("readback: %v", err)
	}
	if seconds != 90 {
		t.Errorf("seconds = %d, want 90 (first write wins)", seconds)
	}
	if kind != "railpack" {
		t.Errorf("kind = %q, want railpack (first write wins)", kind)
	}

	// NOT NULL on account_id / app_id / finished_at.
	for _, col := range []string{"account_id", "app_id", "finished_at", "seconds"} {
		_, err := pool.Exec(ctx, `
			INSERT INTO public.builder_usage (build_id, `+col+`, app_id, finished_at, kind, seconds)
			VALUES (gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), now(), 'none', 0)`)
		if err == nil {
			t.Errorf("expected NOT NULL violation on %s NULL insert", col)
		}
	}

	// Index exists.
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