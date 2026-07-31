package reposcan

import (
	"sort"
	"testing"
	"testing/fstest"
)

// TestDetectWorkspaces_PnpmMonorepo covers the canonical pnpm
// workspace shape: a top-level packages/* glob with three members.
// Each member carries a marker (Dockerfile OR language marker)
// and so qualifies as a workload. README-only dirs are filtered.
func TestDetectWorkspaces_PnpmMonorepo(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"pnpm-workspace.yaml": &fstest.MapFile{
			Data: []byte("packages:\n  - \"packages/*\"\n"),
		},
		"packages/api/Dockerfile":   &fstest.MapFile{Data: []byte("FROM scratch")},
		"packages/web/Dockerfile":   &fstest.MapFile{Data: []byte("FROM scratch")},
		"packages/lib/package.json": &fstest.MapFile{Data: []byte("{}")},
		"packages/docs/README.md":   &fstest.MapFile{Data: []byte("# docs\n")},
	}
	seeds, _, err := detectWorkspaces(fsys)
	if err != nil {
		t.Fatalf("detectWorkspaces: %v", err)
	}
	namesGot := make([]string, len(seeds))
	for i, s := range seeds {
		namesGot[i] = s.name
	}
	sort.Strings(namesGot)
	if !equalSet(namesGot, []string{"api", "web", "lib"}) {
		t.Errorf("seed names = %v, want {api,web,lib} (lib has package.json marker; docs is README-only)", namesGot)
	}
}

// TestDetectWorkspaces_TurboRepo — turbo.json pipeline keys
// emit workloads keyed by name (not directory path).
func TestDetectWorkspaces_TurboRepo(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"turbo.json": &fstest.MapFile{
			Data: []byte("{\"pipeline\":{\"build\":{},\"lint\":{},\"test\":{}}}\n"),
		},
	}
	seeds, _, err := detectWorkspaces(fsys)
	if err != nil {
		t.Fatalf("detectWorkspaces: %v", err)
	}
	// turbo.json pipeline keys aren't directories by default; the
	// marker check is satisfied if a file with the same name as
	// the key exists at root. None exist here, so no workloads.
	// That's correct — Turbo relies on naming conventions
	// (apps/* / packages/*) above the pipeline.
	if len(seeds) != 0 {
		t.Errorf("expected 0 seeds for bare turbo.json with no marker dirs, got %v",
			names(seeds))
	}
}

// TestDetectConvention_DotDirs — services/auth/ + services/payments/
// both have a Dockerfile → 2 workloads; services/lib/ with
// package.json is also a workload (package.json is a valid
// language marker — see §3); services/empty (no marker at all)
// is dropped.
func TestDetectConvention_DotDirs(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"services/auth/Dockerfile":     &fstest.MapFile{Data: []byte("FROM scratch")},
		"services/payments/Dockerfile": &fstest.MapFile{Data: []byte("FROM scratch")},
		"services/lib/package.json":    &fstest.MapFile{Data: []byte("{}")},
		"services/empty/README.md":     &fstest.MapFile{Data: []byte("# nothing here\n")},
	}
	seeds, _, err := detectConvention(fsys)
	if err != nil {
		t.Fatalf("detectConvention: %v", err)
	}
	namesGot := make([]string, len(seeds))
	for i, s := range seeds {
		namesGot[i] = s.name
	}
	sort.Strings(namesGot)
	if !equalSet(namesGot, []string{"auth", "payments", "lib"}) {
		t.Errorf("seed names = %v, want {auth,payments,lib}", namesGot)
	}
}

// TestDetectConvention_AppsAndPkgs — both `apps/` and `packages/`
// are walked; members with go.mod, pyproject.toml, Cargo.toml, or
// pom.xml are eligible too.
func TestDetectConvention_AppsAndPkgs(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"apps/web/Dockerfile":     &fstest.MapFile{Data: []byte("FROM scratch")},
		"apps/api/go.mod":         &fstest.MapFile{Data: []byte("module api\n")},
		"packages/cli/Cargo.toml": &fstest.MapFile{Data: []byte("[package]\nname = \"cli\"\n")},
		"packages/old/README.md":  &fstest.MapFile{Data: []byte("# old\n")},
	}
	seeds, _, err := detectConvention(fsys)
	if err != nil {
		t.Fatalf("detectConvention: %v", err)
	}
	namesGot := make([]string, len(seeds))
	for i, s := range seeds {
		namesGot[i] = s.name
	}
	sort.Strings(namesGot)
	want := []string{"web", "api", "cli"}
	if !equalSet(namesGot, want) {
		t.Errorf("seed names = %v, want %v (README-only dir excluded)", namesGot, want)
	}
}

// TestTierFloor_BareRepo — only a Dockerfile at root. Tiers 1-3
// produce no workloads; Scan() must emit the Tier-4 root-floor
// workload `name="app"`.
func TestTierFloor_BareRepo(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"Dockerfile": &fstest.MapFile{Data: []byte("FROM scratch")},
		"main.go":    &fstest.MapFile{Data: []byte("package main\n")},
	}
	r, err := Scan(fsys)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(r.Workloads) != 1 || r.Workloads[0].Name != "app" {
		t.Errorf("workloads = %v, want one app", r.Workloads)
	}
	if r.Workloads[0].Tier != TierSingle {
		t.Errorf("tier = %s, want single", r.Workloads[0].Tier)
	}
}
