//go:build !no_pg

// Migration-apply test for 00069 (ADR-049 §B.2 + §B.4). Pins the
// (account_id, minute DESC) and (minute) indexes on usage_minutes
// and asserts the column list / ordering so a future migration that
// adds columns and forgets to extend the index trips the gate.

package migrations_test

import (
	"testing"

	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00069_MeteringOpsSurfaces(t *testing.T) {
	pool := pgtest.Open(t)
	defer pool.Close()

	want := map[string]string{
		"usage_minutes_account_minute_idx": "CREATE INDEX usage_minutes_account_minute_idx ON public.usage_minutes USING btree (account_id, minute DESC)",
		"usage_minutes_minute_idx":         "CREATE INDEX usage_minutes_minute_idx ON public.usage_minutes USING btree (minute)",
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
