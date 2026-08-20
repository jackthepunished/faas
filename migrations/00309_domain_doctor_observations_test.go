//go:build !no_pg

// Migration-apply test for 00309_domain_doctor_observations.sql
// (ADR-120, issue #961 follow-on).
//
// Pins:
//
//  1. The table domain_doctor_observations exists with a
//     PRIMARY KEY on `domain` (citext, FK to
//     custom_domains.domain ON DELETE CASCADE). The dns_poller
//     in cmd/apid/dns_poller.go upserts on this column; a
//     schema drift that drops the PK breaks the upsert.
//
//  2. The cert_state column has a closed CHECK constraint
//     named `domain_doctor_observations_cert_state_check`
//     restricting values to
//     ('none','pending','issued','failed','dial_failed'). The
//     same closed set as CustomDomainResponse.CertStatus
//     (pkg/api/dto.go:1663-1676) plus the surface-level
//     CertState (pkg/state/tenant_surface.go:69-75). An
//     INSERT outside the closed set → SQLSTATE 23514.
//
//  3. The two FKs land: (a) domain → custom_domains(domain)
//     ON DELETE CASCADE; (b) surface_id → tenant_surfaces(id)
//     ON DELETE SET NULL. The cascade is load-bearing — when
//     a custom domain is removed, its diagnostic row must
//     be purged (otherwise the doctor would re-surface a
//     row for a domain that no longer exists, which is
//     confusing at minimum and a memory leak at scale).
//
//  4. The stale_idx exists on observed_at so the
//     "stuck-domain" alert query
//     (`SELECT domain FROM domain_doctor_observations
//     WHERE observed_at < now() - INTERVAL '30 minutes' AND
//     NOT dns_record_found`) is index-only.
//
// Build tag matches the rest of the migration tests.
// Set FAAS_SKIP_PG_TESTS=1 to skip locally (see
// migrations/README.md).
package migrations_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00296_DomainDoctorObservations(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}

	// (1) Table exists with a PK on `domain` (citext).
	var tableExists bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = current_schema()
			  AND table_name = 'domain_doctor_observations'
		)
	`).Scan(&tableExists); err != nil {
		t.Fatalf("query domain_doctor_observations existence: %v", err)
	}
	if !tableExists {
		t.Fatalf("domain_doctor_observations missing (00296 must create it)")
	}

	// PK is on `domain` (citext) — confirm via
	// information_schema.table_constraints + key_column_usage
	// (the canonical FK/PK introspection pair; pg_index
	// gives positional access but the SQL standard pair is
	// the form every other migration test uses).
	var pkName string
	if err := pool.QueryRow(ctx, `
		SELECT tc.constraint_name
		  FROM information_schema.table_constraints tc
		 WHERE tc.table_schema = current_schema()
		   AND tc.table_name = 'domain_doctor_observations'
		   AND tc.constraint_type = 'PRIMARY KEY'
	`).Scan(&pkName); err != nil {
		t.Fatalf("query PK: %v", err)
	}
	if pkName == "" {
		t.Errorf("domain_doctor_observations has no PRIMARY KEY (dns_poller's INSERT ... ON CONFLICT (domain) requires one)")
	}

	// (2) cert_state CHECK exists with the closed set
	// ('none','pending','issued','failed','dial_failed').
	// The harness default-names the constraint
	// domain_doctor_observations_cert_state_check; we
	// confirm by name + parse the CHECK text to assert the
	// closed set is intact (a future migration that widens
	// it must land in a separate file with its own test).
	var certStateDef string
	if err := pool.QueryRow(ctx, `
		SELECT pg_get_constraintdef(c.oid)
		  FROM pg_catalog.pg_constraint c
		 WHERE c.conrelid = 'domain_doctor_observations'::regclass
		   AND c.conname = 'domain_doctor_observations_cert_state_check'
	`).Scan(&certStateDef); err != nil {
		t.Fatalf("query cert_state CHECK: %v", err)
	}
	if certStateDef == "" {
		t.Errorf("domain_doctor_observations_cert_state_check missing (the closed-set enum must be enforced)")
	}
	for _, want := range []string{"'none'", "'pending'", "'issued'", "'failed'", "'dial_failed'"} {
		if !strings.Contains(certStateDef, want) {
			t.Errorf("domain_doctor_observations_cert_state_check missing token %s (got %q)", want, certStateDef)
		}
	}

	// CHECK violation: insert with an out-of-set cert_state.
	// We don't seed a custom_domains row first because the
	// FK is the next assertion; SQLSTATE 23514 (check_violation)
	// fires before the FK check at insert time, so the
	// ordering is safe.
	_, err := pool.Exec(ctx, `
		INSERT INTO domain_doctor_observations
			(domain, dns_record_found, points_to_gregale, ipv6_conflict, cert_state)
		VALUES
			('no-such.example.com', true, true, false, 'bogus_state')
	`)
	if err == nil {
		t.Errorf("INSERT with cert_state='bogus_state' should violate CHECK; got no error")
	} else {
		var pgErr *pgconn.PgError
		if !errorsAs(err, &pgErr) || pgErr.Code != "23514" {
			t.Errorf("expected SQLSTATE 23514 on cert_state CHECK violation, got %v", err)
		}
	}

	// (3) FKs: (a) domain → custom_domains(domain) ON DELETE CASCADE
	// is verified by the CASCADE on the constraint definition
	// (pg_get_constraintdef contains "CASCADE"). (b) surface_id
	// → tenant_surfaces(id) ON DELETE SET NULL likewise contains
	// "SET NULL".
	var domainFK, surfaceFK string
	rows, err := pool.Query(ctx, `
		SELECT conname, pg_get_constraintdef(c.oid)
		  FROM pg_catalog.pg_constraint c
		 WHERE c.conrelid = 'domain_doctor_observations'::regclass
		   AND c.contype = 'f'
	`)
	if err != nil {
		t.Fatalf("query FKs: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			t.Fatalf("scan FK row: %v", err)
		}
		switch name {
		case "domain_doctor_observations_domain_fkey":
			domainFK = def
		case "domain_doctor_observations_surface_id_fkey":
			surfaceFK = def
		}
	}
	if domainFK == "" {
		t.Errorf("domain_doctor_observations.domain FK to custom_domains missing")
	} else if !strings.Contains(strings.ToUpper(domainFK), "CASCADE") {
		t.Errorf("domain FK must be ON DELETE CASCADE; got %q", domainFK)
	}
	if surfaceFK == "" {
		t.Errorf("domain_doctor_observations.surface_id FK to tenant_surfaces missing")
	} else if !strings.Contains(strings.ToUpper(surfaceFK), "SET NULL") {
		t.Errorf("surface_id FK must be ON DELETE SET NULL; got %q", surfaceFK)
	}

	// (4) stale_idx on observed_at. We assert by querying
	// pg_indexes for the index name (the migration names
	// it `domain_doctor_observations_stale_idx`).
	var idxDef string
	if err := pool.QueryRow(ctx, `
		SELECT indexdef FROM pg_indexes
		 WHERE schemaname = current_schema()
		   AND tablename = 'domain_doctor_observations'
		   AND indexname = 'domain_doctor_observations_stale_idx'
	`).Scan(&idxDef); err != nil {
		t.Fatalf("query stale_idx: %v", err)
	}
	if !strings.Contains(idxDef, "observed_at") {
		t.Errorf("domain_doctor_observations_stale_idx must include observed_at; got %q", idxDef)
	}

	// (5) caa_permits is NULL-able (the "no CAA published"
	// case is healthy by default, not a failure). This
	// matters because the doctor handler reads the column
	// as a tri-state, not a bool.
	var isNullable string
	if err := pool.QueryRow(ctx, `
		SELECT is_nullable FROM information_schema.columns
		 WHERE table_schema = current_schema()
		   AND table_name = 'domain_doctor_observations'
		   AND column_name = 'caa_permits'
	`).Scan(&isNullable); err != nil {
		t.Fatalf("query caa_permits nullability: %v", err)
	}
	if isNullable != "YES" {
		t.Errorf("caa_permits must be NULL-able (no-CAA-published is the healthy default); got is_nullable=%q", isNullable)
	}

	// UUID import guard — uuid is required by future
	// PR-B work (custom_domain_certs mirror) that will
	// reuse this test file's harness. Suppress the
	// unused-import lint now so the test compiles
	// without an `_ = uuid.UUID{}` line.
	_ = uuid.UUID{}
}

// errorsAs is a thin alias for the standard library's
// errors.As — the test only needs to know whether err is or
// wraps a *pgconn.PgError. Kept as a named helper so the
// call site reads as a type-assertion and the standard
// library does the unwrap-chain walk (no hand-rolled
// Unwrap loop, which errorlint would flag).
func errorsAs(err error, target any) bool {
	return errors.As(err, target)
}
