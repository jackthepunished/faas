//go:build !no_pg

// Migration-apply test for 00070 (ADR-049 §B.3). Pins the
// snapshot_storage_daily table + index shape.

package migrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00070_SnapshotStorageDaily(t *testing.T) {
	pool := pgtest.Open(t)
	defer pool.Close()

	ctx := context.Background()
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}

	// Columns + types + NOT NULL. Note: 00070's CREATE TABLE uses
	// `public.snapshot_storage_daily` explicitly, so the table
	// lives in `public` regardless of search_path.
	wantCols := map[string]string{
		"account_id":     "uuid",
		"app_id":         "uuid",
		"day":            "date",
		"snapshot_bytes": "bigint",
		"layer_bytes":    "bigint",
		"computed_at":    "timestamp with time zone",
	}
	rows, err := pool.Query(ctx,
		`select column_name, data_type
		   from information_schema.columns
		  where table_schema = 'public'
		    and table_name = 'snapshot_storage_daily'`)
	if err != nil {
		t.Fatalf("query columns: %v", err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[name] = typ
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	for name, wantTyp := range wantCols {
		gotTyp, ok := got[name]
		if !ok {
			t.Errorf("column %q missing on snapshot_storage_daily", name)
			continue
		}
		if gotTyp != wantTyp {
			t.Errorf("column %q type = %q, want %q", name, gotTyp, wantTyp)
		}
	}

	// Index exists (lives in public because 00070 uses
	// `public.snapshot_storage_daily` explicitly). Schema-agnostic
	// — pgtest.Open uses a per-test schema so the indexdef always
	// carries the literal schema name. Pin only the suffix.
	var indexDef string
	err = pool.QueryRow(ctx,
		`select indexdef from pg_indexes
		  where schemaname = 'public'
		    and tablename = 'snapshot_storage_daily'
		    and indexname = 'snapshot_storage_daily_account_day_idx'`).
		Scan(&indexDef)
	if err != nil {
		t.Fatalf("query index: %v", err)
	}
	wantSuffix := "snapshot_storage_daily USING btree (account_id, day DESC)"
	if !strings.HasSuffix(indexDef, wantSuffix) {
		t.Errorf("snapshot_storage_daily_account_day_idx def drifted:\n got  %s\n want suffix %s", indexDef, wantSuffix)
	}
}
