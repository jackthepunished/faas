package reposcan

import (
	"io/fs"
)

// detectWorkspaces returns one workloadSeed per workspace-graph
// member that ALSO carries a Dockerfile or language marker. A
// library-only member (e.g. packages/ui with package.json but no
// Dockerfile) is NOT a workload — it's a workspace tool dependency.
// Implementation lives in workspaces.go.
//
// Recognized workspace manifests (impl plan §3):
//   - package.json  (top-level "workspaces": [...])
//   - pnpm-workspace.yaml (top-level "packages": [...])
//   - turbo.json    (pipeline / $pipeline form)
//   - nx.json       (projects)
//   - go.work       (use ( ... ))
//   - Cargo.toml    ([workspace] members) [out of scope this phase]
//
// Each workspace member has its directory treated as RootDir —
// the merge rule later pairs on (RootDir, Name) so a workspace
// "packages/auth" with auth-name does NOT collide with a Tier-3
// "services/auth" convention of the same name.
//
// Pure: no fsys error escalates to Scan. A missing manifest file
// is a quiet skip.
func detectWorkspaces(fsys fs.FS) ([]workloadSeed, []string, error) {
	return detectWorkspacesImpl(fsys)
}

// detectConvention scans the documented Tier-3 root subdirectories
// (services/, apps/, packages/, cmd/) and emits one workloadSeed
// per member that carries a Dockerfile or language marker. Tier 3
// is exactly the directory-shape heuristic; it's why the Phase 3
// confirm table exists (always ask before provisioning).
func detectConvention(fsys fs.FS) ([]workloadSeed, []string, error) {
	return detectConventionImpl(fsys)
}
