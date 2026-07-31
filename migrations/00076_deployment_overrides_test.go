//go:build !no_pg

// Migration-apply test for 00076 (deploy-time overrides on deployments,
// issue #460 / ADR-053). Pins the override columns:
//
//  1. The migration set applies cleanly through 00076.
//  2. Each override column accepts the canonical shape (text[] for
//     entrypoint/cmd, jsonb for env/env_secrets/healthcheck, int for
//     port) and round-trips.
//  3. Nullable: a deployment with no override writes NULL for every
//     override column (regression check — pre-PR rows still work).
//  4. Replay-safe: ADD COLUMN IF NOT EXISTS makes a second MigrateUp
//     no-op (PR #377 / ADR-041).
//
// Slot note: HEAD is at 00075 (Node 24 / Python 3.13 widening), so 00076
// is the next free slot at PR creation time. The migration is slot-
// agnostic — only the filename and the test function name carry the
// literal slot. If a sibling PR grabs 00076 first, renumber per
// `migrations/README.md` and update this test's filename + ApplyUp
// range.
//
// Build tag matches the rest of the migration tests; set
// FAAS_SKIP_PG_TESTS=1 to skip locally (see migrations/README.md).
package migrations_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00076_DeploymentOverrides(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// Seed UUIDs use the `d` prefix in the 4th group (e.g. `0d00076`)
	// because the readable `000076` form is a coincidental collision
	// with the migration slot number and could shadow another
	// migration's pin in a future test. The `d` flag stands for
	// "deployment overrides" and makes the source obvious to a
	// reader scanning the test fixtures.

	// (1) Apply through 00076. A regression that drops a slot between
	// 1 and 76 surfaces here before the per-assertion pins.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 1 and 76)", err)
	}

	// (2) Seed an account + app. The literal UUIDs are fixed across
	// reruns so the seed is idempotent.
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-0000d000076',
		        'deploy-overrides-test@example.com', 'hobby', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, idle_timeout_s, status, created_at)
		values ('00000000-0000-0000-0000-0000d000176',
		        '00000000-0000-0000-0000-0000d000076',
		        'overrides-test-app', 'function', 256, 1, 30, 'active', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed app: %v", err)
	}

	// (3) Insert a deployment with the canonical override shape.
	// Each column type is exercised:
	//   override_entrypoint    text[]   → ARRAY['/usr/bin/node','/srv/app.js']
	//   override_cmd          text[]   → ARRAY['--port','9090']
	//   override_env          jsonb    → {"LOG_LEVEL":"debug"}
	//   override_env_secrets  jsonb    → {"DB_URL":"secret:db-url"}
	//   override_port         int      → 9090
	//   override_healthcheck  jsonb    → {"path":"/healthz","interval_s":5,"timeout_s":2,"retries":3}
	envJSON := `{"LOG_LEVEL":"debug"}`
	envSecretsJSON := `{"DB_URL":"secret:db-url"}`
	healthcheckJSON := `{"path":"/healthz","interval_s":5,"timeout_s":2,"retries":3}`
	if _, err := pool.Exec(ctx, `
		insert into deployments (
			id, app_id, image_digest, kind, status,
			override_entrypoint, override_cmd, override_env,
			override_env_secrets, override_port, override_healthcheck,
			created_at
		) values (
			'00000000-0000-0000-0000-0000d000276',
			'00000000-0000-0000-0000-0000d000176',
			'sha256:0000000000000000000000000000000000000000000000000000000000000001',
			'image', 'pending',
			ARRAY['/usr/bin/node','/srv/app.js'],
			ARRAY['--port','9090'],
			$1::jsonb, $2::jsonb, 9090, $3::jsonb,
			now()
		)
	`, envJSON, envSecretsJSON, healthcheckJSON); err != nil {
		t.Fatalf("insert deployment with full override shape: %v", err)
	}

	// (4) Round-trip each column type.
	var (
		gotEntrypoint                                          []string
		gotCmd                                                 []string
		gotPort                                                int
		gotEnvRaw, gotEnvSecretsRaw, gotHealthcheckRaw        []byte
	)
	if err := pool.QueryRow(ctx, `
		select override_entrypoint, override_cmd, override_port,
		       override_env, override_env_secrets, override_healthcheck
		from deployments
		where id = '00000000-0000-0000-0000-0000d000276'
	`).Scan(&gotEntrypoint, &gotCmd, &gotPort,
		&gotEnvRaw, &gotEnvSecretsRaw, &gotHealthcheckRaw); err != nil {
		t.Fatalf("read back deployment overrides: %v", err)
	}
	if len(gotEntrypoint) != 2 || gotEntrypoint[0] != "/usr/bin/node" || gotEntrypoint[1] != "/srv/app.js" {
		t.Errorf("override_entrypoint round-trip = %v, want [/usr/bin/node /srv/app.js]", gotEntrypoint)
	}
	if len(gotCmd) != 2 || gotCmd[0] != "--port" || gotCmd[1] != "9090" {
		t.Errorf("override_cmd round-trip = %v, want [--port 9090]", gotCmd)
	}
	if gotPort != 9090 {
		t.Errorf("override_port round-trip = %d, want 9090", gotPort)
	}
	var gotEnv map[string]string
	if err := json.Unmarshal(gotEnvRaw, &gotEnv); err != nil {
		t.Fatalf("override_env jsonb unmarshal: %v (raw=%s)", err, gotEnvRaw)
	}
	if gotEnv["LOG_LEVEL"] != "debug" {
		t.Errorf("override_env[LOG_LEVEL] = %q, want debug", gotEnv["LOG_LEVEL"])
	}
	var gotEnvSecrets map[string]string
	if err := json.Unmarshal(gotEnvSecretsRaw, &gotEnvSecrets); err != nil {
		t.Fatalf("override_env_secrets jsonb unmarshal: %v (raw=%s)", err, gotEnvSecretsRaw)
	}
	if gotEnvSecrets["DB_URL"] != "secret:db-url" {
		t.Errorf("override_env_secrets[DB_URL] = %q, want secret:db-url", gotEnvSecrets["DB_URL"])
	}
	var gotHealthcheck map[string]any
	if err := json.Unmarshal(gotHealthcheckRaw, &gotHealthcheck); err != nil {
		t.Fatalf("override_healthcheck jsonb unmarshal: %v (raw=%s)", err, gotHealthcheckRaw)
	}
	if gotHealthcheck["path"] != "/healthz" {
		t.Errorf("override_healthcheck[path] = %v, want /healthz", gotHealthcheck["path"])
	}

	// (5) Nullable: a deployment with no override writes NULL for every
	// override column. Mirrors the pre-PR rows so they still load.
	if _, err := pool.Exec(ctx, `
		insert into deployments (
			id, app_id, image_digest, kind, status, created_at
		) values (
			'00000000-0000-0000-0000-0000d000376',
			'00000000-0000-0000-0000-0000d000176',
			'sha256:0000000000000000000000000000000000000000000000000000000000000002',
			'image', 'pending', now()
		)
	`); err != nil {
		t.Fatalf("insert deployment with no override: %v", err)
	}
	var (
		nullEntrypoint    []string
		nullCmd           []string
		nullPort          *int
		nullEnv, nullSecs, nullHC []byte
	)
	if err := pool.QueryRow(ctx, `
		select override_entrypoint, override_cmd, override_port,
		       override_env, override_env_secrets, override_healthcheck
		from deployments
		where id = '00000000-0000-0000-0000-0000d000376'
	`).Scan(&nullEntrypoint, &nullCmd, &nullPort,
		&nullEnv, &nullSecs, &nullHC); err != nil {
		t.Fatalf("read back null-override deployment: %v", err)
	}
	if nullEntrypoint != nil {
		t.Errorf("override_entrypoint on null-override deployment = %v, want nil", nullEntrypoint)
	}
	if nullCmd != nil {
		t.Errorf("override_cmd on null-override deployment = %v, want nil", nullCmd)
	}
	if nullPort != nil {
		t.Errorf("override_port on null-override deployment = %d, want nil", *nullPort)
	}
	if nullEnv != nil {
		t.Errorf("override_env on null-override deployment = %s, want nil", nullEnv)
	}
	if nullSecs != nil {
		t.Errorf("override_env_secrets on null-override deployment = %s, want nil", nullSecs)
	}
	if nullHC != nil {
		t.Errorf("override_healthcheck on null-override deployment = %s, want nil", nullHC)
	}

	// (6) Replay safety: a second MigrateUp is a no-op (the migration
	// uses ADD COLUMN IF NOT EXISTS). PR #377 / ADR-041 contract.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay-safety: second MigrateUp failed: %v", err)
	}
}
