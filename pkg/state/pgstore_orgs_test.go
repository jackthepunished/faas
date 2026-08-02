//go:build !no_pg

// PgStore coverage tests for the org schema surface (issue #190 /
// ADR-061, PR 2). Sister file to memstore_orgs_test.go so each method
// has parity coverage on both backends. The migration shape is
// pinned by 00095_orgs_memberships_invitations_test.go — this file
// proves the PgStore adapter wires the right SQL to those constraints.
//
// Build tag matches migrations/*_test.go — set FAAS_SKIP_PG_TESTS=1 to
// skip locally (see migrations/README.md).

package state

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func pgStoreTimeNow() time.Time { return time.Now().UTC() }

func newPgStore(t *testing.T) *PgStore {
	t.Helper()
	pool := pgtest.Open(t)
	if err := db.MigrateUp(context.Background(), pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}
	return NewPgStore(pool)
}

func TestPgStore_OrgSurface_RoundTripsAndLastOwnerGuard(t *testing.T) {
	pool := pgtest.Open(t)
	if err := db.MigrateUp(context.Background(), pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}
	s := NewPgStore(pool)
	ctx := context.Background()

	// Seed an account (the slug / FK on org_memberships.account_id does
	// not require a pre-existing account to insert into orgs, but
	// AddOrgMember does).
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-0000000000b1', 'pg-org-1@x.com', 'free', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-0000000000b2', 'pg-org-2@x.com', 'free', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed account 2: %v", err)
	}

	o, err := s.CreateOrg(ctx, newTestOrg("pg-org"))
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	// Case-insensitive unique: orgs_slug_uniq uses lower(slug). The
	// slug regex forces lowercase, so two shape-valid slugs that
	// lower-case the same value can only be inserted via raw SQL.
	// Probe the unique contract directly to pin what the sqlc surface
	// relies on.
	if _, err := pool.Exec(ctx, `
		insert into orgs (slug, name) values ('pg-org', 'Other Org')
	`); err == nil {
		t.Errorf("orgs_slug_uniq: duplicate slug should be rejected")
	}

	got, err := s.OrgBySlug(ctx, "pg-org")
	if err != nil {
		t.Fatalf("OrgBySlug: %v", err)
	}
	if got.ID != o.ID {
		t.Errorf("slug round-trip id mismatch")
	}

	if err := s.AddOrgMember(ctx, o.ID, "00000000-0000-0000-0000-0000000000b1", OrgRoleOwner, nil); err != nil {
		t.Fatalf("add owner: %v", err)
	}
	if err := s.AddOrgMember(ctx, o.ID, "00000000-0000-0000-0000-0000000000b2", OrgRoleOwner, nil); !errors.Is(err, ErrOrgLastOwner) {
		t.Errorf("second owner: err = %v, want ErrOrgLastOwner", err)
	}
	if err := s.AddOrgMember(ctx, o.ID, "00000000-0000-0000-0000-0000000000b2", OrgRoleAdmin, nil); err != nil {
		t.Errorf("add admin: %v", err)
	}
	if err := s.RemoveOrgMember(ctx, o.ID, "00000000-0000-0000-0000-0000000000b1"); !errors.Is(err, ErrOrgLastOwner) {
		t.Errorf("remove sole owner: err = %v, want ErrOrgLastOwner", err)
	}

	got2, err := s.ListOrgMembers(ctx, o.ID)
	if err != nil {
		t.Fatalf("ListOrgMembers: %v", err)
	}
	if len(got2) != 2 {
		t.Errorf("members = %d, want 2", len(got2))
	}

	if err := s.SoftDeleteOrg(ctx, o.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	final, err := s.OrgByID(ctx, o.ID)
	if err != nil {
		t.Fatalf("OrgByID after delete: %v", err)
	}
	if !final.DeletedPending {
		t.Errorf("DeletedPending false after soft delete")
	}
}

func TestPgStore_ConsumeOrgInvitation_HappyPath(t *testing.T) {
	// Smoke test for the tx-heavy acceptance path. The full cap / email /
	// race matrix is exercised by the migration test (which pins SQL
	// invariants) and the memstore parity test (which pins store semantics);
	// this file proves the PgStore wires both to the same tx semantics.
	pool := pgtest.Open(t)
	if err := db.MigrateUp(context.Background(), pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}
	s := NewPgStore(pool)
	ctx := context.Background()

	for i, id := range []string{
		"00000000-0000-0000-0000-0000000000c1",
		"00000000-0000-0000-0000-0000000000c2",
	} {
		email := []string{"c1@x.com", "c2@x.com"}[i]
		if _, err := pool.Exec(ctx, `
			insert into accounts (id, email, plan, created_at)
			values ($1::uuid, $2, 'free', now())
			on conflict (id) do nothing
		`, id, email); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	o, err := s.CreateOrg(ctx, Org{
		Slug: "consume-test", Name: "Consume Test",
		Plan: api.PlanHobby, Status: OrgStatusActive,
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := s.AddOrgMember(ctx, o.ID, "00000000-0000-0000-0000-0000000000c1", OrgRoleOwner, nil); err != nil {
		t.Fatalf("add owner: %v", err)
	}
	tokenHash := sha256.New().Sum([]byte("cap-token"))
	inv := OrgInvitation{
		OrgID:     o.ID,
		Email:     "c2@x.com",
		Role:      OrgRoleDeveloper,
		TokenHash: tokenHash[:],
		ExpiresAt: pgStoreTimeNow().Add(time.Hour),
	}
	if _, err := s.CreateOrgInvitation(ctx, inv); err != nil {
		t.Fatalf("create invitation: %v", err)
	}
	acceptor := Account{
		ID:    "00000000-0000-0000-0000-0000000000c2",
		Email: "c2@x.com",
	}
	mem, returned, err := s.ConsumeOrgInvitation(ctx, tokenHash[:], acceptor)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if mem.Role != OrgRoleDeveloper {
		t.Errorf("consume membership role = %s, want developer", mem.Role)
	}
	if returned.ConsumedAt == nil {
		t.Errorf("returned invitation has nil ConsumedAt")
	}
}

func TestPgStore_ExpireOrgInvitations_Counts(t *testing.T) {
	pool := pgtest.Open(t)
	if err := db.MigrateUp(context.Background(), pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}
	s := NewPgStore(pool)
	ctx := context.Background()

	// Seed an account + org.
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-0000000000d1', 'expire@x.com', 'free', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	o, err := s.CreateOrg(ctx, newTestOrg("expire-pg"))
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	pastHash := sha256.New().Sum([]byte("past-pg"))
	futureHash := sha256.New().Sum([]byte("future-pg"))
	if _, err := s.CreateOrgInvitation(ctx, OrgInvitation{
		OrgID: o.ID, Email: "p@x.com", Role: OrgRoleViewer,
		TokenHash: pastHash[:], ExpiresAt: pgStoreTimeNow().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("seed past: %v", err)
	}
	if _, err := s.CreateOrgInvitation(ctx, OrgInvitation{
		OrgID: o.ID, Email: "f@x.com", Role: OrgRoleViewer,
		TokenHash: futureHash[:], ExpiresAt: pgStoreTimeNow().Add(time.Hour),
	}); err != nil {
		t.Fatalf("seed future: %v", err)
	}
	n, err := s.ExpireOrgInvitations(ctx, pgStoreTimeNow())
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n != 1 {
		t.Errorf("expired count = %d, want 1", n)
	}
}
