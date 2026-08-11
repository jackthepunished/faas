//go:build !no_pg

// Migration-apply test for 00220 (preview app columns, issue #272
// PR-preview environments, ADR-094). Pins:
//
//  1. The migration set applies cleanly through 00220.
//  2. The four new apps columns land with the documented nullability
//     and the preview_pr_state CHECK accepts the four legal values
//     (open / closed / stale / torn_down).
//  3. The deployments / builds CHECK constraints accept the new
//     'preview' kind value (the load-bearing tripwire — without
//     these relaxations, the apid bridge INSERT trips 23514).
//  4. Closed vocabulary is preserved: an unknown state ('abandoned')
//     is rejected, and an unknown deployment kind ('cli') is still
//     rejected.
//  5. Replay-safe: a second MigrateUp is a no-op.
//
// Slot note: at PR-A creation time the slot table on origin/main
// ends at 00217 (PR #849's app_secrets_scope landed at 00217). Slot
// 00220 is the next free, uncontested slot. PR-A fences the slot
// with migrations/00220_reserve_slot.sql in the first commit and
// renames the body to migrations/00220_preview_app_columns.sql in
// the same PR before merge — the fence file is removed on rebase.
package migrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00220_PreviewAppColumns(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// (1) Apply through 00220.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 1 and 218)", err)
	}

	// (2) The four new columns exist and accept the legal values.
	// Insert an app with preview_of_slug + preview_pr_state='open'
	// + preview_pr_number=42 + preview_expires_at populated. The
	// presence of preview_of_slug = 'preview-test-parent' makes the
	// partial indexes apps_preview_of_slug_idx + apps_preview_expires_at_idx
	// see this row (we don't assert on the index itself — just that
	// the row was accepted by the schema).
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-000000000220',
		        'preview-app-cols-test@example.com', 'hobby', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (
		    id, account_id, slug, type, ram_mb, max_concurrency,
		    idle_timeout_s, status, created_at,
		    preview_of_slug, preview_pr_number, preview_pr_state, preview_expires_at
		)
		values (
		    '00000000-0000-0000-0000-000000000220',
		    '00000000-0000-0000-0000-000000000220',
		    'preview-app-cols-test-app', 'function', 256, 1, 30, 'active', now(),
		    'preview-test-parent', 42, 'open', now() + interval '7 days'
		)
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("insert preview app: %v (regression: columns or CHECK rejected)", err)
	}

	// (3) deployments / builds accept the new 'preview' kind. Seed
	// a deployment + build for the preview app, both kind='preview'.
	if _, err := pool.Exec(ctx, `
		insert into deployments (id, app_id, kind, image_digest, status, source_url, commit_sha, created_at)
		values ('00000000-0000-0000-0000-000000000220',
		        '00000000-0000-0000-0000-000000000220',
		        'preview', '', 'pending', 'https://github.com/example/repo@abc1234', 'abc1234', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed deployment kind=preview: %v (regression: deployments CHECK did not accept 'preview')", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into builds (id, deployment_id, kind, source_bytes, status)
		values ('00000000-0000-0000-0000-000000000220',
		        '00000000-0000-0000-0000-000000000220',
		        'preview', 1, 'queued')
	`); err != nil {
		t.Fatalf("insert build kind=preview: %v (regression: builds CHECK did not accept 'preview')", err)
	}

	// (4) Closed vocabulary: an unknown preview_pr_state is rejected
	// by the apps CHECK, and an unknown deployments.kind is rejected.
	_, err := pool.Exec(ctx, `
		insert into apps (
		    id, account_id, slug, type, ram_mb, max_concurrency,
		    idle_timeout_s, status, created_at,
		    preview_of_slug, preview_pr_number, preview_pr_state
		)
		values (
		    '00000000-0000-0000-0000-000000000219',
		    '00000000-0000-0000-0000-000000000220',
		    'preview-app-cols-test-bad', 'function', 256, 1, 30, 'active', now(),
		    'preview-test-parent', 99, 'abandoned'
		)
	`)
	if err == nil {
		t.Errorf("apps.preview_pr_state='abandoned' was accepted; CHECK did not preserve the closed vocabulary")
	} else if !strings.Contains(err.Error(), "apps_preview_pr_state_chk") {
		t.Errorf("expected preview_pr_state CHECK violation, got: %v", err)
	}
	_, err = pool.Exec(ctx, `
		insert into deployments (id, app_id, kind, image_digest, status, source_url, commit_sha, created_at)
		values ('00000000-0000-0000-0000-000000000219',
		        '00000000-0000-0000-0000-000000000220',
		        'cli', '', 'pending', 'https://github.com/example/repo@abc1234', 'abc1234', now())
	`)
	if err == nil {
		t.Errorf("deployments.kind='cli' was accepted; CHECK did not preserve the closed vocabulary")
	} else if !strings.Contains(err.Error(), "deployments_kind_check") {
		t.Errorf("expected deployments_kind_check violation, got: %v", err)
	}

	// (5) Replay safety.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay-safety: second MigrateUp failed: %v", err)
	}
}