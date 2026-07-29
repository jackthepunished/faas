//go:build !no_pg

// Migration-apply test for 00069 (ADR-049 §B.2 + §B.4). Pins the
// (account_id, minute DESC) and (minute) indexes on usage_minutes
// and asserts the column list / ordering so a future migration that
// adds columns and forgets to extend the index trips the gate.

package migrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00069_MeteringOpsSurfaces(t *testing.T) {
	pool := pgtest.Open(t)
	defer pool.Close()
	if err := db.MigrateUp(context.Background(), pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}

	// Schema-agnostic assertion: pgtest.Open uses a per-test schema
	// (e.g. faas_test_e29aacc9bd8d3477), so the indexdef will always
	// carry the literal schema name. We pin the suffix after `ON `.
	want := map[string]string{
		"usage_minutes_account_minute_idx": "usage_minutes USING btree (account_id, minute DESC)",
		"usage_minutes_minute_idx":         "usage_minutes USING btree (minute)",
	}
	rows, err := pool.Query(t.Context(),
		`select indexname, indexdef from pg_indexes
		   where schemaname = current_schema()
		     and tablename = 'usage_minutes'
		     and indexname in ('usage_minutes_account_minute_idx', 'usage_minutes_minute_idx')`)
	if err != nil {
		t.Fatalf("query indexes: %v", err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			t.Fatalf("scan: %v", err)
		}
		// Strip the "ON <schema>." prefix pg stamps on indexdef.
		if i := strings.Index(def, "ON "); i >= 0 {
			rest := def[i+3:]
			if j := strings.Index(rest, "."); j >= 0 {
				def = rest[j+1:]
			}
		}
		got[name] = def
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	for name, wantDef := range want {
		gotDef, ok := got[name]
		if !ok {
			t.Errorf("index %q missing (got %v indexes for usage_minutes)", name, got)
			continue
		}
		if gotDef != wantDef {
			t.Errorf("index %q definition drifted:\n got  %s\n want %s", name, gotDef, wantDef)
		}
	}
}
