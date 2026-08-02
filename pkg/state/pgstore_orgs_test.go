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

	"github.com/jackc/pgx/v5/pgxpool"
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
	s := newPgStore(t)
	pool := s.pool
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

// pgStoreSeedAccounts inserts N test accounts (deterministic UUIDs +
// emails) so the rest of the coverage suite can reuse them. Idempotent
// (ON CONFLICT DO NOTHING).
func pgStoreSeedAccounts(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := pool.Exec(ctx, `
			insert into accounts (id, email, plan, created_at)
			values ($1::uuid, $2, 'free', now())
			on conflict (id) do nothing
		`, id, "pg-"+id+"@x.com"); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
}

// TestPgStore_OrgLookups_ByPersonalAndSlug exercises OrgByPersonalAccount
// and OrgBySlug happy + ErrNotFound paths that the roundtrip test does
// not cover.
func TestPgStore_OrgLookups_ByPersonalAndSlug(t *testing.T) {
	s := newPgStore(t)
	pool := s.pool
	ctx := context.Background()
	pgStoreSeedAccounts(t, ctx, pool, "00000000-0000-0000-0000-0000000000d2")

	// Personal orgs use a slug derived from the account id short-form so
	// it fits the orgs_slug_shape check (3-32 chars). MemStore does not
	// enforce this constraint, but PgStore does.
	shortAcct := "d2"
	personalFixture := newTestPersonalOrg("00000000-0000-0000-0000-0000000000d2")
	personalFixture.Slug = "p-" + shortAcct
	personal, err := s.CreateOrg(ctx, personalFixture)
	if err != nil {
		t.Fatalf("create personal: %v", err)
	}

	got, err := s.OrgByPersonalAccount(ctx, "00000000-0000-0000-0000-0000000000d2")
	if err != nil {
		t.Fatalf("OrgByPersonalAccount: %v", err)
	}
	if got.ID != personal.ID {
		t.Errorf("OrgByPersonalAccount id = %s, want %s", got.ID, personal.ID)
	}

	if _, err := s.OrgByPersonalAccount(ctx, "00000000-0000-0000-0000-000000000099"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing personal account: err = %v, want ErrNotFound", err)
	}

	// ListOrgsForAccount must JOIN through active memberships; adding
	// the personal account as the owner membership surfaces the personal
	// org in the returned slice.
	if err := s.AddOrgMember(ctx, personal.ID, "00000000-0000-0000-0000-0000000000d2", OrgRoleOwner, nil); err != nil {
		t.Fatalf("add owner membership: %v", err)
	}
	list, err := s.ListOrgsForAccount(ctx, "00000000-0000-0000-0000-0000000000d2")
	if err != nil {
		t.Fatalf("ListOrgsForAccount: %v", err)
	}
	if len(list) != 1 || list[0].ID != personal.ID {
		t.Errorf("ListOrgsForAccount = %+v, want exactly [%s]", list, personal.ID)
	}

	// OrgBySlug miss path.
	if _, err := s.OrgBySlug(ctx, "no-such-slug-zzz"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing slug: err = %v, want ErrNotFound", err)
	}
}

// TestPgStore_OrgUpdates_PlanStatusDelete exercises UpdateOrgPlan,
// UpdateOrgStatus, and SoftDeleteOrg happy + ErrNotFound paths.
func TestPgStore_OrgUpdates_PlanStatusDelete(t *testing.T) {
	s := newPgStore(t)
	ctx := context.Background()

	o, err := s.CreateOrg(ctx, newTestOrg("update-pg"))
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	if err := s.UpdateOrgPlan(ctx, o.ID, api.PlanPro); err != nil {
		t.Fatalf("UpdateOrgPlan: %v", err)
	}
	got, err := s.OrgByID(ctx, o.ID)
	if err != nil {
		t.Fatalf("OrgByID: %v", err)
	}
	if got.Plan != api.PlanPro {
		t.Errorf("after UpdateOrgPlan plan = %s, want pro", got.Plan)
	}

	if err := s.UpdateOrgStatus(ctx, o.ID, OrgStatusPastDue); err != nil {
		t.Fatalf("UpdateOrgStatus: %v", err)
	}
	got, err = s.OrgByID(ctx, o.ID)
	if err != nil {
		t.Fatalf("OrgByID: %v", err)
	}
	if got.Status != OrgStatusPastDue {
		t.Errorf("after UpdateOrgStatus status = %s, want past_due", got.Status)
	}

	// ErrNotFound on bogus org id.
	if err := s.UpdateOrgPlan(ctx, "00000000-0000-0000-0000-000000000099", api.PlanHobby); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateOrgPlan bogus id: err = %v, want ErrNotFound", err)
	}
	if err := s.UpdateOrgStatus(ctx, "00000000-0000-0000-0000-000000000099", OrgStatusActive); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateOrgStatus bogus id: err = %v, want ErrNotFound", err)
	}

	if err := s.SoftDeleteOrg(ctx, o.ID); err != nil {
		t.Fatalf("SoftDeleteOrg: %v", err)
	}
	got, err = s.OrgByID(ctx, o.ID)
	if err != nil {
		t.Fatalf("OrgByID: %v", err)
	}
	if !got.DeletedPending {
		t.Errorf("after SoftDeleteOrg DeletedPending = false")
	}

	if err := s.SoftDeleteOrg(ctx, "00000000-0000-0000-0000-000000000099"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SoftDeleteOrg bogus id: err = %v, want ErrNotFound", err)
	}
}

// TestPgStore_MembershipOps_AllBranches exercises AddOrgMember (success +
// dup), UpdateOrgMemberRole (success + ErrNotFound), OrgMemberByAccount
// (success + ErrNotFound), and ListOrgMembers happy path.
func TestPgStore_MembershipOps_AllBranches(t *testing.T) {
	s := newPgStore(t)
	pool := s.pool
	ctx := context.Background()
	pgStoreSeedAccounts(t, ctx, pool,
		"00000000-0000-0000-0000-0000000000e1",
		"00000000-0000-0000-0000-0000000000e2",
	)

	o, err := s.CreateOrg(ctx, newTestOrg("memops-pg"))
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := s.AddOrgMember(ctx, o.ID, "00000000-0000-0000-0000-0000000000e1", OrgRoleOwner, nil); err != nil {
		t.Fatalf("add owner: %v", err)
	}

	// AddOrgMember dup path: re-adding same (org,account) → ErrConflict.
	if err := s.AddOrgMember(ctx, o.ID, "00000000-0000-0000-0000-0000000000e1", OrgRoleAdmin, nil); !errors.Is(err, ErrConflict) {
		t.Errorf("AddOrgMember dup: err = %v, want ErrConflict", err)
	}

	if err := s.AddOrgMember(ctx, o.ID, "00000000-0000-0000-0000-0000000000e2", OrgRoleDeveloper, nil); err != nil {
		t.Fatalf("add dev: %v", err)
	}

	// OrgMemberByAccount happy + miss.
	mem, err := s.OrgMemberByAccount(ctx, o.ID, "00000000-0000-0000-0000-0000000000e2")
	if err != nil {
		t.Fatalf("OrgMemberByAccount: %v", err)
	}
	if mem.Role != OrgRoleDeveloper {
		t.Errorf("role = %s, want developer", mem.Role)
	}
	if _, err := s.OrgMemberByAccount(ctx, o.ID, "00000000-0000-0000-0000-0000000000ff"); !errors.Is(err, ErrNotFound) {
		t.Errorf("OrgMemberByAccount miss: err = %v, want ErrNotFound", err)
	}

	// UpdateOrgMemberRole happy + ErrNotFound.
	if err := s.UpdateOrgMemberRole(ctx, o.ID, "00000000-0000-0000-0000-0000000000e2", OrgRoleAdmin); err != nil {
		t.Fatalf("UpdateOrgMemberRole: %v", err)
	}
	mem, err = s.OrgMemberByAccount(ctx, o.ID, "00000000-0000-0000-0000-0000000000e2")
	if err != nil {
		t.Fatalf("OrgMemberByAccount: %v", err)
	}
	if mem.Role != OrgRoleAdmin {
		t.Errorf("role after update = %s, want admin", mem.Role)
	}
	if err := s.UpdateOrgMemberRole(ctx, o.ID, "00000000-0000-0000-0000-0000000000ff", OrgRoleViewer); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateOrgMemberRole miss: err = %v, want ErrNotFound", err)
	}

	// ListOrgMembers happy path: 2 active members.
	listed, err := s.ListOrgMembers(ctx, o.ID)
	if err != nil {
		t.Fatalf("ListOrgMembers: %v", err)
	}
	if len(listed) != 2 {
		t.Errorf("ListOrgMembers len = %d, want 2", len(listed))
	}
}

// TestPgStore_InvitationOps_AllBranches exercises CreateOrgInvitation +
// OrgInvitationByTokenHash + RevokeOrgInvitation + ListOrgInvitationsForOrg.
func TestPgStore_InvitationOps_AllBranches(t *testing.T) {
	s := newPgStore(t)
	ctx := context.Background()

	o, err := s.CreateOrg(ctx, newTestOrg("inviteops-pg"))
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	// OrgInvitationByTokenHash miss path (the roundtrip test covers hit).
	if _, err := s.OrgInvitationByTokenHash(ctx, []byte("never-stored")); !errors.Is(err, ErrNotFound) {
		t.Errorf("OrgInvitationByTokenHash miss: err = %v, want ErrNotFound", err)
	}

	hash := sha256.New().Sum([]byte("inviteops-token"))
	inv := OrgInvitation{
		OrgID:     o.ID,
		Email:     "inv@x.com",
		Role:      OrgRoleDeveloper,
		TokenHash: hash[:],
		ExpiresAt: pgStoreTimeNow().Add(time.Hour),
	}
	if _, err := s.CreateOrgInvitation(ctx, inv); err != nil {
		t.Fatalf("CreateOrgInvitation: %v", err)
	}

	got, err := s.OrgInvitationByTokenHash(ctx, hash[:])
	if err != nil {
		t.Fatalf("OrgInvitationByTokenHash hit: %v", err)
	}
	if got.Email != "inv@x.com" {
		t.Errorf("invitation email = %s, want inv@x.com", got.Email)
	}

	// ListOrgInvitationsForOrg: one pending.
	listed, err := s.ListOrgInvitationsForOrg(ctx, o.ID)
	if err != nil {
		t.Fatalf("ListOrgInvitationsForOrg: %v", err)
	}
	if len(listed) != 1 {
		t.Errorf("ListOrgInvitationsForOrg len = %d, want 1", len(listed))
	}

	// RevokeOrgInvitation happy path.
	if err := s.RevokeOrgInvitation(ctx, hash[:], "00000000-0000-0000-0000-000000000099"); err != nil {
		t.Fatalf("RevokeOrgInvitation: %v", err)
	}
	got, err = s.OrgInvitationByTokenHash(ctx, hash[:])
	if err != nil {
		t.Fatalf("OrgInvitationByTokenHash post-revoke: %v", err)
	}
	if got.RevokedAt == nil {
		t.Errorf("after revoke: RevokedAt is nil")
	}

	// RevokeOrgInvitation miss path: a token that never existed is
	// indistinguishable from one already revoked — both report zero
	// rows updated, which the implementation surfaces as
	// ErrOrgInvitationInvalid (parity with ConsumeOrgInvitation).
	if err := s.RevokeOrgInvitation(ctx, []byte("never-stored-x"), "00000000-0000-0000-0000-000000000099"); !errors.Is(err, ErrOrgInvitationInvalid) {
		t.Errorf("RevokeOrgInvitation miss: err = %v, want ErrOrgInvitationInvalid", err)
	}
}

// TestPgStore_ConsumeOrgInvitation_ErrorPaths exercises the email
// mismatch + already-consumed branches that the happy-path test does
// not hit. These flow through pgx error mapping → store sentinel errors.
func TestPgStore_ConsumeOrgInvitation_ErrorPaths(t *testing.T) {
	s := newPgStore(t)
	pool := s.pool
	ctx := context.Background()
	pgStoreSeedAccounts(t, ctx, pool,
		"00000000-0000-0000-0000-0000000000f1",
		"00000000-0000-0000-0000-0000000000f2",
	)

	o, err := s.CreateOrg(ctx, newTestOrg("consume-err-pg"))
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := s.AddOrgMember(ctx, o.ID, "00000000-0000-0000-0000-0000000000f1", OrgRoleOwner, nil); err != nil {
		t.Fatalf("add owner: %v", err)
	}

	hash := sha256.New().Sum([]byte("consume-err-token"))
	if _, err := s.CreateOrgInvitation(ctx, OrgInvitation{
		OrgID:     o.ID,
		Email:     "expected@x.com",
		Role:      OrgRoleViewer,
		TokenHash: hash[:],
		ExpiresAt: pgStoreTimeNow().Add(time.Hour),
	}); err != nil {
		t.Fatalf("create invitation: %v", err)
	}

	// Email mismatch → ErrOrgInvitationInvalid.
	mismatchAcct := Account{ID: "00000000-0000-0000-0000-0000000000f2", Email: "different@x.com"}
	if _, _, err := s.ConsumeOrgInvitation(ctx, hash[:], mismatchAcct); !errors.Is(err, ErrOrgInvitationInvalid) {
		t.Errorf("email mismatch: err = %v, want ErrOrgInvitationInvalid", err)
	}

	// Consume once successfully to flip consumed_at, then re-consume → ErrOrgInvitationInvalid.
	correctAcct := Account{ID: "00000000-0000-0000-0000-0000000000f2", Email: "expected@x.com"}
	if _, _, err := s.ConsumeOrgInvitation(ctx, hash[:], correctAcct); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if _, _, err := s.ConsumeOrgInvitation(ctx, hash[:], correctAcct); !errors.Is(err, ErrOrgInvitationInvalid) {
		t.Errorf("re-consume: err = %v, want ErrOrgInvitationInvalid", err)
	}
}
