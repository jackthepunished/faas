//go:build !no_pg

// Project-method tests for the ADR-050 Phase 1 storage seam. Mirrors
// the pattern in pgstore_test.go (pgStore + seed utility) and the
// memstore parity tests in memstore_projects_test.go.
//
// Pins the Phase 1 store contract:
//
//   - CreateProject returns ErrConflict on duplicate (account_id, slug)
//     and ErrNotFound when the owning account is gone.
//   - ProjectByRepo resolves a backfilled (install_id, repo) pair.
//   - ProjectByRepo returns ErrNotFound on a missing row.
//   - AppsForProject accounts-scopes (cross-account project reads 404).
//   - SetProjectScanSource is monotonic upward; downgrades fail with
//     ErrScanSourceDowngrade; same-tier is a no-op.

package state_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/state"
)

func TestPg_CreateProject_UniqueSlug(t *testing.T) {
	s, ctx := pgStore(t)
	acct, err := s.CreateAccount(ctx, "proj-uniq@example.test", "free")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	p, err := s.CreateProject(ctx, state.Project{
		AccountID: acct.ID,
		Slug:      "phase1-pg-uniq",
	})
	if err != nil {
		t.Fatalf("CreateProject happy: %v", err)
	}
	if p.ID == "" {
		t.Errorf("CreateProject ID = %q, want non-empty", p.ID)
	}
	if p.ScanSource != state.ProjectScanSourceUnknown {
		t.Errorf("default ScanSource = %q, want 'unknown'", p.ScanSource)
	}

	// Second insert: same (account_id, slug) trips unique violation.
	_, err = s.CreateProject(ctx, state.Project{
		AccountID: acct.ID,
		Slug:      "phase1-pg-uniq",
	})
	if !errors.Is(err, state.ErrConflict) {
		t.Errorf("CreateProject dup = %v, want ErrConflict", err)
	}
}

func TestPg_CreateProject_AccountMissing(t *testing.T) {
	s, ctx := pgStore(t)
	_, err := s.CreateProject(ctx, state.Project{
		AccountID: "00000000-0000-0000-0000-000000000000", // unknown uuid
		Slug:      "phase1-pg-orphan",
	})
	if !errors.Is(err, state.ErrNotFound) {
		t.Errorf("CreateProject with missing account = %v, want ErrNotFound", err)
	}
}

func TestPg_ProjectByRepo_BackfilledHit(t *testing.T) {
	s, ctx := pgStore(t)
	acct, err := s.CreateAccount(ctx, "proj-backfill@example.test", "free")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	installID := int64(90010)
	repoFull := "acme/phase1-pg-backfill"

	p, err := s.CreateProject(ctx, state.Project{
		AccountID:        acct.ID,
		Slug:             "phase1-pg-backfill",
		RepoFullName:     repoFull,
		ProductionBranch: "main",
		InstallID:        installID,
		ScanSource:       state.ProjectScanSourceCompose,
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	got, err := s.ProjectByRepo(ctx, acct.ID, installID, repoFull)
	if err != nil {
		t.Fatalf("ProjectByRepo: %v", err)
	}
	if got.ID != p.ID {
		t.Errorf("ProjectByRepo.ID = %q, want %q", got.ID, p.ID)
	}
	if got.ScanSource != state.ProjectScanSourceCompose {
		t.Errorf("ScanSource = %q, want 'compose'", got.ScanSource)
	}
}

func TestPg_ProjectByRepo_Missing(t *testing.T) {
	s, ctx := pgStore(t)
	acct, err := s.CreateAccount(ctx, "proj-missing@example.test", "free")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	_, err = s.ProjectByRepo(ctx, acct.ID, 99999, "does/not-exist")
	if !errors.Is(err, state.ErrNotFound) {
		t.Errorf("ProjectByRepo missing = %v, want ErrNotFound", err)
	}
}

func TestPg_AppsForProject_AccountScoped(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	acctA, err := s.CreateAccount(ctx, "proj-scope-a@example.test", "free")
	if err != nil {
		t.Fatalf("CreateAccount A: %v", err)
	}
	acctB, err := s.CreateAccount(ctx, "proj-scope-b@example.test", "free")
	if err != nil {
		t.Fatalf("CreateAccount B: %v", err)
	}

	p, err := s.CreateProject(ctx, state.Project{
		AccountID: acctA.ID,
		Slug:      "phase1-pg-scope",
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	// Direct INSERT to set project_id + workload_name on the app row —
	// avoids wiring CreateApp through the project_id seam (Phase 3).
	_, err = pool.Exec(ctx, `
		insert into apps (id, account_id, slug, ram_mb, max_concurrency,
		                  project_id, workload_name, workload_class)
		values (gen_random_uuid(), $1, 'phase1-pg-scope-member', 256, 1,
		        $2, 'web', 'http')
	`, acctA.ID, p.ID)
	if err != nil {
		t.Fatalf("seed app: %v", err)
	}

	// Same-account read returns the seeded app.
	got, err := s.AppsForProject(ctx, acctA.ID, p.ID)
	if err != nil {
		t.Fatalf("AppsForProject same-account: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("AppsForProject same-account len = %d, want 1", len(got))
	}

	// Cross-account read returns ErrNotFound (does not leak membership).
	_, err = s.AppsForProject(ctx, acctB.ID, p.ID)
	if !errors.Is(err, state.ErrNotFound) {
		t.Errorf("AppsForProject cross-account = %v, want ErrNotFound", err)
	}
}

// TestPg_SetProjectScanSource_MonotonicUp verifies the load-bearing
// invariant: scan_source ranks upward only. A downgrade is rejected
// with ErrScanSourceDowngrade; same-tier is a no-op (updated_at still
// moves forward).
func TestPg_SetProjectScanSource_MonotonicUp(t *testing.T) {
	s, ctx := pgStore(t)
	acct, err := s.CreateAccount(ctx, "proj-mono@example.test", "free")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	p, err := s.CreateProject(ctx, state.Project{
		AccountID:  acct.ID,
		Slug:       "phase1-pg-mono",
		ScanSource: state.ProjectScanSourceSingle,
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	// single → compose: ok.
	p2, err := s.SetProjectScanSource(ctx, p.ID, state.ProjectScanSourceCompose)
	if err != nil {
		t.Fatalf("SetProjectScanSource single→compose: %v", err)
	}
	if p2.ScanSource != state.ProjectScanSourceCompose {
		t.Errorf("after single→compose = %q, want 'compose'", p2.ScanSource)
	}

	// compose → single: rejected.
	_, err = s.SetProjectScanSource(ctx, p.ID, state.ProjectScanSourceSingle)
	if !errors.Is(err, state.ErrScanSourceDowngrade) {
		t.Errorf("SetProjectScanSource compose→single = %v, want ErrScanSourceDowngrade", err)
	}

	// compose → compose: same-tier no-op returns the row.
	p3, err := s.SetProjectScanSource(ctx, p.ID, state.ProjectScanSourceCompose)
	if err != nil {
		t.Fatalf("SetProjectScanSource compose→compose: %v", err)
	}
	if p3.ScanSource != state.ProjectScanSourceCompose {
		t.Errorf("same-tier write ScanSource = %q, want 'compose'", p3.ScanSource)
	}

	// unknown → render: ok (render is rank 8, unknown is rank 0).
	_, err = s.SetProjectScanSource(ctx, p.ID, state.ProjectScanSourceRender)
	if err != nil {
		t.Errorf("SetProjectScanSource unknown→render: %v", err)
	}
}

// TestPg_ListProjectsForAccount verifies the per-account listing:
// returns every project under the account, scoped to that account
// only (a project under acctB does not leak into acctA's list).
// Mirrors the memstore parity test in memstore_test.go.
func TestPg_ListProjectsForAccount(t *testing.T) {
	s, ctx := pgStore(t)

	acctA, err := s.CreateAccount(ctx, "proj-list-a@example.test", "free")
	if err != nil {
		t.Fatalf("CreateAccount A: %v", err)
	}
	acctB, err := s.CreateAccount(ctx, "proj-list-b@example.test", "free")
	if err != nil {
		t.Fatalf("CreateAccount B: %v", err)
	}

	// 3 under acctA, 1 under acctB.
	for _, slug := range []string{"phase1-pg-list-a1", "phase1-pg-list-a2", "phase1-pg-list-a3"} {
		if _, err := s.CreateProject(ctx, state.Project{
			AccountID: acctA.ID,
			Slug:      slug,
		}); err != nil {
			t.Fatalf("seed %s: %v", slug, err)
		}
	}
	if _, err := s.CreateProject(ctx, state.Project{
		AccountID: acctB.ID,
		Slug:      "phase1-pg-list-b1",
	}); err != nil {
		t.Fatalf("seed acctB project: %v", err)
	}

	got, err := s.ListProjectsForAccount(ctx, acctA.ID)
	if err != nil {
		t.Fatalf("ListProjectsForAccount: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("ListProjectsForAccount acctA len = %d, want 3", len(got))
	}
	// Account scoping: every returned row belongs to acctA.
	for _, p := range got {
		if p.AccountID != acctA.ID {
			t.Errorf("ListProjectsForAccount returned row %s account_id=%q, want %q",
				p.ID, p.AccountID, acctA.ID)
		}
	}
	// Slug uniqueness: every result has the acctA namespace.
	for _, p := range got {
		if !strings.HasPrefix(p.Slug, "phase1-pg-list-a") {
			t.Errorf("ListProjectsForAccount leaked slug %q into acctA", p.Slug)
		}
	}

	// Empty account returns nil slice, not error.
	emptyAcct, err := s.CreateAccount(ctx, "proj-list-empty@example.test", "free")
	if err != nil {
		t.Fatalf("CreateAccount empty: %v", err)
	}
	gotEmpty, err := s.ListProjectsForAccount(ctx, emptyAcct.ID)
	if err != nil {
		t.Errorf("ListProjectsForAccount empty: %v", err)
	}
	if len(gotEmpty) != 0 {
		t.Errorf("empty list len = %d, want 0", len(gotEmpty))
	}
}
