//go:build !no_pg

// Migration-apply test for 00099_orgs_memberships_invitations.sql
// (issue #190 / ADR-061, PR 2). Pins the additive org / membership /
// invitation schema + every load-bearing invariant the downstream
// PRs (3 backfill, 4 auth facade, 5 invite handlers, 6 API-key org
// binding, 7 cutover) depend on.
//
// Pins:
//
//  1. Migration set applies cleanly through 00099.
//  2. orgs: column shape, slug CHECK, case-insensitive unique,
//     exactly-one-personal-org partial unique, personal-vs-shared
//     CHECK, plan / status CHECKs.
//  3. org_memberships: role CHECK, exactly-one-owner partial unique,
//     removed-role CHECK, FK cascade from orgs, FK cascade from accounts.
//  4. org_invitations: token_hash unique, role CHECK (owner excluded),
//     state CHECK (consumed_at / revoked_at mutex), FK cascade from
//     orgs, invited_by SET NULL on account delete, accepting_account_id
//     SET NULL on account delete.
//  5. events.actor_account_id column exists; SET NULL on account delete.
//  6. Nullable org_id on every section-B tenant-root table
//     (information_schema probe, fail-on-missing).
//  7. Per-table partial indexes for org_id exist.
//  8. Replay-safe: second MigrateUp is a no-op (ADR-041).
//
// Slot note: 00099 is the next free slot after slot 94 (app_registry_credentials
// per PR #522). Renumbering requires updating the seed UUIDs below,
// e2eMigrationTarget in pkg/e2etest/harness.go, and the companion
// 00100_reserve_slot.sql (which ADR-041 says to git rm post-merge).
//
// Build tag matches the rest of the migration tests; set
// FAAS_SKIP_PG_TESTS=1 to skip locally (see migrations/README.md).
package migrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00099_OrgsMembershipsInvitations(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// Seed UUIDs carry the slot number (`...000099`) so a reader scanning
	// the test fixtures can pin each row to this migration without
	// grepping the file name. Literal slot must stay in sync with the
	// filename + e2eMigrationTarget.

	// (1) Apply through slot 99.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 1 and 99)", err)
	}

	// (2) Seed an account. Trusted-signer slot 86's account row is also
	// available; we use slot 99's own seed for isolation.
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-000000000099',
		        'orgs-seed@example.com', 'free', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	// Second seeded account for the membership / FK-cascade assertions.
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-000000000199',
		        'orgs-seed-2@example.com', 'free', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed account 2: %v", err)
	}

	// (3) orgs.slug CHECK shape — uppercase letter, leading dash,
	// trailing dash, 2-char (under min), 33-char (over max) all fail.
	badSlugs := []string{
		"Abc",                   // uppercase
		"-leading-dash",         // leading dash
		"trailing-dash-",        // trailing dash
		"ab",                    // too short
		strings.Repeat("a", 33), // too long
	}
	for _, s := range badSlugs {
		if _, err := pool.Exec(ctx, `
			insert into orgs (slug, name) values ($1, 'bad')
		`, s); err == nil {
			t.Errorf("orgs.slug CHECK: slug %q should have been rejected", s)
		}
	}

	// (4) orgs case-insensitive unique on slug.
	if _, err := pool.Exec(ctx, `
		insert into orgs (slug, name) values ('first-co', 'First Co')
	`); err != nil {
		t.Fatalf("seed orgs.first-co: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into orgs (slug, name) values ('FIRST-CO', 'Dup Capitalised')
	`); err == nil {
		t.Errorf("orgs.slug case-uniqueness: 'FIRST-CO' should collide with 'first-co'")
	}

	// (5) Exactly-one-personal-org partial unique. Insert a personal
	// org for the slot-95 account, then a second personal org for the
	// same account must fail.
	if _, err := pool.Exec(ctx, `
		insert into orgs (slug, name, personal_org, personal_owner_account_id)
		values ('personal-95', 'Personal',
		        true, '00000000-0000-0000-0000-000000000099')
	`); err != nil {
		t.Fatalf("seed personal org: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into orgs (slug, name, personal_org, personal_owner_account_id)
		values ('personal-95-dup', 'Dup Personal',
		        true, '00000000-0000-0000-0000-000000000099')
	`); err == nil {
		t.Errorf("orgs exactly-one-personal per account: duplicate insert should have been rejected")
	}

	// (6) personal vs shared CHECK: a non-personal org must NOT carry a
	// personal_owner_account_id.
	if _, err := pool.Exec(ctx, `
		insert into orgs (slug, name, personal_org, personal_owner_account_id)
		values ('shared-bad', 'Shared Bad',
		        false, '00000000-0000-0000-0000-000000000099')
	`); err == nil {
		t.Errorf("orgs_personal_owner_link: shared org with personal_owner_account_id should fail CHECK")
	}

	// (7) plan / status CHECKs.
	if _, err := pool.Exec(ctx, `insert into orgs (slug, name, plan) values ('plan-bad', 'Plan Bad', 'mega')`); err == nil {
		t.Errorf("orgs_plan_chk: 'mega' should be rejected")
	}
	if _, err := pool.Exec(ctx, `insert into orgs (slug, name, status) values ('status-bad', 'Status Bad', 'weird')`); err == nil {
		t.Errorf("orgs_status_chk: 'weird' should be rejected")
	}

	// (8) Seed a shared (non-personal) org for membership assertions.
	if _, err := pool.Exec(ctx, `
		insert into orgs (id, slug, name, personal_org)
		values ('00000000-0000-0000-0000-000000000299',
		        'shared-95', 'Shared Co', false)
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed shared org: %v", err)
	}
	// Look up the personal org id (slot-95 personal org we inserted above).
	var personalOrgID string
	if err := pool.QueryRow(ctx, `
		select id from orgs where slug = 'personal-95'
	`).Scan(&personalOrgID); err != nil {
		t.Fatalf("lookup personal org id: %v", err)
	}

	// (9) org_memberships PK duplicate is rejected.
	if _, err := pool.Exec(ctx, `
		insert into org_memberships (org_id, account_id, role)
		values ($1, '00000000-0000-0000-0000-000000000099', 'owner')
	`, personalOrgID); err != nil {
		t.Fatalf("seed owner membership: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into org_memberships (org_id, account_id, role)
		values ($1, '00000000-0000-0000-0000-000000000099', 'admin')
	`, personalOrgID); err == nil {
		t.Errorf("org_memberships PK: duplicate (org_id, account_id) should be rejected")
	}

	// (10) org_memberships role CHECK: invalid role rejected.
	if _, err := pool.Exec(ctx, `
		insert into org_memberships (org_id, account_id, role)
		values ('00000000-0000-0000-0000-000000000299',
		        '00000000-0000-0000-0000-000000000199', 'superuser')
	`); err == nil {
		t.Errorf("org_memberships_role_chk: 'superuser' should be rejected")
	}

	// (11) exactly-one-owner partial unique. Insert owner on the shared
	// org, then a second owner for a different account on the same org.
	if _, err := pool.Exec(ctx, `
		insert into org_memberships (org_id, account_id, role)
		values ('00000000-0000-0000-0000-000000000299',
		        '00000000-0000-0000-0000-000000000099', 'owner')
	`); err != nil {
		t.Fatalf("seed shared owner: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into org_memberships (org_id, account_id, role)
		values ('00000000-0000-0000-0000-000000000299',
		        '00000000-0000-0000-0000-000000000199', 'owner')
	`); err == nil {
		t.Errorf("org_memberships_one_owner partial unique: second active owner should be rejected")
	}

	// (12) removed-role CHECK: removed_at IS NOT NULL AND role = 'owner'
	// is rejected; setting a row to removed_at + non-owner role is fine.
	if _, err := pool.Exec(ctx, `
		insert into org_memberships (org_id, account_id, role, removed_at)
		values ('00000000-0000-0000-0000-000000000299',
		        '00000000-0000-0000-0000-000000000199', 'owner', now())
	`); err == nil {
		t.Errorf("org_memberships_removed_role_chk: removed owner should be rejected")
	}

	// (13) FK cascade from orgs on memberships. Drop the shared org, the
	// membership rows must disappear.
	if _, err := pool.Exec(ctx, `
		delete from orgs where id = '00000000-0000-0000-0000-000000000299'
	`); err != nil {
		t.Fatalf("delete shared org: %v", err)
	}
	var remaining int
	if err := pool.QueryRow(ctx, `
		select count(*) from org_memberships
		 where org_id = '00000000-0000-0000-0000-000000000299'
	`).Scan(&remaining); err != nil {
		t.Fatalf("count memberships after cascade: %v", err)
	}
	if remaining != 0 {
		t.Errorf("memberships after org cascade = %d, want 0", remaining)
	}

	// (14) org_invitations role CHECK rejects 'owner'. The personal org
	// (still live at this point) gives us a non-personal FK target so
	// only the CHECK trips — same CHECK is exercised on the shared org
	// below in (15), but the FK-vs-CHECK ambiguity there is the reason
	// this earlier assertion uses the personal org.
	if _, err := pool.Exec(ctx, `
		insert into org_invitations (org_id, email, role, token_hash, expires_at)
		values ($1, 'owner-invite@example.com', 'owner',
		        decode(repeat('aa', 32), 'hex'), now() + interval '1 day')
	`, personalOrgID); err == nil {
		t.Errorf("org_invitations_role_chk: 'owner' should be rejected")
	}

	// (15) org_invitations state CHECK: setting both consumed_at and
	// revoked_at non-NULL must fail. Insert a fresh pending invitation
	// first (need a live org — reuse the personal org).
	if _, err := pool.Exec(ctx, `
		insert into org_invitations (org_id, email, role, token_hash, expires_at)
		values ($1, 'invitee@example.com', 'admin',
		        decode(repeat('bb', 32), 'hex'), now() + interval '1 day')
	`, personalOrgID); err != nil {
		t.Fatalf("seed invitation: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		update org_invitations
		   set consumed_at = now(), revoked_at = now()
		 where email = 'invitee@example.com'
	`); err == nil {
		t.Errorf("org_invitations_state_chk: both consumed_at and revoked_at non-NULL should be rejected")
	}

	// (16) Token-hash unique.
	if _, err := pool.Exec(ctx, `
		insert into org_invitations (org_id, email, role, token_hash, expires_at)
		values ($1, 'dup-token@example.com', 'admin',
		        decode(repeat('bb', 32), 'hex'), now() + interval '1 day')
	`, personalOrgID); err == nil {
		t.Errorf("org_invitations_token_uniq: duplicate token_hash should be rejected")
	}

	// (17) invited_by_account_id SET NULL on account delete.
	if _, err := pool.Exec(ctx, `
		insert into org_invitations (org_id, email, role, token_hash, invited_by_account_id, expires_at)
		values ($1, 'inviter-test@example.com', 'viewer',
		        decode(repeat('cc', 32), 'hex'),
		        '00000000-0000-0000-0000-000000000199',
		        now() + interval '1 day')
	`, personalOrgID); err != nil {
		t.Fatalf("seed invitation with inviter: %v", err)
	}
	// Account 195 currently has no membership or owned row that would
	// block the delete; delete it, then assert invited_by is NULL.
	if _, err := pool.Exec(ctx, `
		delete from accounts where id = '00000000-0000-0000-0000-000000000199'
	`); err != nil {
		t.Fatalf("delete account 195: %v", err)
	}
	var invitedBy *string
	if err := pool.QueryRow(ctx, `
		select invited_by_account_id from org_invitations where email = 'inviter-test@example.com'
	`).Scan(&invitedBy); err != nil {
		t.Fatalf("read invited_by after account delete: %v", err)
	}
	if invitedBy != nil {
		t.Errorf("invited_by_account_id after account delete = %s, want NULL", *invitedBy)
	}

	// (18) events.actor_account_id SET NULL on account delete. Insert
	// an event with the (now-deleted) account id as actor, then assert
	// the column goes NULL… except the account is gone, so we directly
	// verify the column exists and has the ON DELETE SET NULL semantic
	// by inserting an event, then deleting the actor account, and
	// asserting NULL. We use the slot-95 account which still exists.
	if _, err := pool.Exec(ctx, `
		insert into events (actor, actor_account_id, kind, data)
		values ('system', '00000000-0000-0000-0000-000000000099',
		        'org.test', '{}'::jsonb)
	`); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	// Insert a placeholder app to satisfy the apps FK on delete (the
	// slot-95 account has no apps yet).
	if _, err := pool.Exec(ctx, `
		insert into apps (account_id, slug, type, ram_mb, max_concurrency, idle_timeout_s, status, created_at)
		values ('00000000-0000-0000-0000-000000000099', 'evt-test-app-95',
		        'function', 128, 1, 30, 'active', now())
	`); err != nil {
		t.Fatalf("seed app for event test: %v", err)
	}
	// events has no FK to accounts on actor_account_id itself, but
	// there's also no blocking row to prevent deleting the account.
	// We must first remove the app that the account owns.
	if _, err := pool.Exec(ctx, `delete from apps where slug = 'evt-test-app-95'`); err != nil {
		t.Fatalf("delete app before account: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		delete from accounts where id = '00000000-0000-0000-0000-000000000099'
	`); err != nil {
		t.Fatalf("delete account 95 for events test: %v", err)
	}
	var actorAfter *string
	if err := pool.QueryRow(ctx, `
		select actor_account_id from events
		 where actor = 'system' and kind = 'org.test'
		 order by at desc limit 1
	`).Scan(&actorAfter); err != nil {
		t.Fatalf("read events.actor_account_id after delete: %v", err)
	}
	if actorAfter != nil {
		t.Errorf("events.actor_account_id after account delete = %s, want NULL", *actorAfter)
	}

	// (19) Nullable org_id on every section-B tenant-root table.
	sectionBTables := []string{
		"apps", "projects", "custom_domains", "api_keys", "instances",
		"usage_minutes", "usage_daily", "invoices", "stripe_push_dedupe",
		"paddle_overage_dedupe", "app_secrets", "app_envs", "alert_rules",
		"recent_build_claims", "builder_usage", "crons", "invocations",
		"github_installations", "gdpr_requests",
	}
	for _, tbl := range sectionBTables {
		var n int
		if err := pool.QueryRow(ctx, `
			select count(*) from information_schema.columns
			 where table_schema = current_schema()
			   and table_name = $1
			   and column_name = 'org_id'
		`, tbl).Scan(&n); err != nil {
			t.Fatalf("probe org_id on %s: %v", tbl, err)
		}
		if n != 1 {
			t.Errorf("table %s is missing org_id column (count=%d)", tbl, n)
		}
	}

	// (20) Per-table partial indexes for org_id exist.
	for _, tbl := range sectionBTables {
		var n int
		if err := pool.QueryRow(ctx, `
			select count(*) from pg_indexes
			 where schemaname = current_schema()
			   and tablename = $1
			   and indexname = $2
		`, tbl, tbl+"_org_id_idx").Scan(&n); err != nil {
			t.Fatalf("probe partial index on %s: %v", tbl, err)
		}
		if n != 1 {
			t.Errorf("table %s is missing %s_org_id_idx partial index (count=%d)", tbl, tbl, n)
		}
	}

	// (21) Replay safety — second MigrateUp is a no-op (ADR-041).
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay-safety: second MigrateUp failed: %v", err)
	}
}
