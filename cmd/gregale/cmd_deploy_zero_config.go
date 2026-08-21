// cmd/gregale/cmd_deploy_zero_config.go — zero-config `gregale deploy`
// entry point (issue #961 / Mega-A PR-1). When the customer types
// `gregale deploy` with no source flag, this handler takes over IF cwd
// is inside a git repo with an `origin` remote.
//
// Flow:
//  1. Walk up to the git root.
//  2. Read the `origin` remote URL and parse (owner, repo).
//  3. Resolve HEAD to a 40-char SHA (recorded as provenance).
//  4. Warn if the working tree is dirty (non-fatal).
//  5. Pack cwd via the existing autoPackCwd path.
//  6. Upload via Client.DeployFromSourceTarball (CLI is the trust root;
//     see docs/adr/0XX-local-tarball-deploy-trust-root.md).
//  7. Stream the build log via the existing streamDeployLogs path.
//
// The handler mirrors cmdDeployRepoSourceRef (--repo X --ref SHA) but
// skips the GitHub install-token path entirely.

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
)

// cmdDeployZeroConfig is the entry point for `gregale deploy` (no
// flags) inside a git repo. Assumes the caller has already checked
// that cwd is inside a git repo.
func cmdDeployZeroConfig(slug, cwd string) int {
	root, err := gitRootFromCwd(cwd)
	if err != nil {
		return printErr("Could not resolve git root", err)
	}

	remoteURL, err := gitRemoteOrigin(root)
	if err != nil {
		return printErr("Could not read git remote 'origin'", err)
	}
	owner, repo, err := parseGitRemoteURL(remoteURL)
	if err != nil {
		// Non-GitHub remotes fall through to a clear error so the
		// operator knows to pass --repo / --ref explicitly or
		// re-point their remote at GitHub.
		return printErr(fmt.Sprintf("Unsupported origin %q", remoteURL), err)
	}

	sha, err := resolveHEAD(root)
	if err != nil {
		return printErr("Could not resolve HEAD", err)
	}

	if dirty, err := isDirtyWorkdir(root); err == nil && dirty {
		PrintOK(osStdout, "Note: working tree has uncommitted changes — deploying HEAD as-is")
	}

	// Issue #977 / ADR-116: auto-capture `git config user.name` as
	// the deployment's deployed_by. operator can override with
	// --deployed-by in the cmdDeployTarball path; the zero-config
	// path doesn't have flags so we read what the user has
	// configured. gitUserName swallows ErrNoGitConfigKey to ""
	// (the "operator never configured git" path) — empty is a
	// valid deployed_by, the column is nullable.
	//
	// Review fix CRIT-2 (issue #977 / ADR-116): non-ErrNoGitConfigKey
	// errors (config-file parse, permission denied, transient git
	// hiccup) are also silently swallowed to "" — mirrors the
	// policy in cmd_deploy_annotations.go:resolveDeployedBy, so a
	// customer who deploys the same project via either path gets
	// the same stamp. The audit row simply lacks a deployed_by and
	// the dashboard renders nothing. Promoting these to a fatal
	// would break §11 cross-path symmetry: a parse-corrupt
	// ~/.gitconfig would block the zero-config path but not the
	// flag path.
	name, err := gitUserName(root)
	if err != nil {
		// log-and-continue: the deployment is still valid, just
		// unannotated. The slug is the audit anchor.
		PrintOK(osStdout, fmt.Sprintf("Note: could not read git config user.name (%v); proceeding without deployed_by", err))
		name = ""
	}
	deployedBy := name

	// Pack cwd. We intentionally reuse autoPackCwd (the same helper
	// the cwd auto-detection branch uses) so the §9 shape invariants
	// (≤10k files, no symlinks, no ../, etc.) stay aligned across
	// the two zero-config paths.
	tmpTar, _, fileCount, err := autoPackCwd(cwd, nil)
	if err != nil {
		return printErr("Could not pack current directory", err)
	}
	defer func() { _ = os.Remove(tmpTar) }()
	PrintProgress(os.Stderr, "packing %d file(s) from %s", fileCount, filepath.Base(cwd))

	client, err := authedClientWithDeployTimeout(5 * time.Minute)
	if err != nil {
		return printErr("Not logged in", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dep, err := client.DeployFromSourceTarball(ctx, slug, mustOpen(tmpTar), filepath.Base(tmpTar), sourceTarballSidecar(owner, repo, sha, deployedBy))
	if err != nil {
		return printErr("Deploy failed", err)
	}

	// Print the deployment id so the operator can grep for it; the
	// build log stream follows on the next line.
	PrintProgress(osStdout, "build queued for %s (deployment %s)", dep.AppID, dep.ID)
	return streamDeployLogs(client, dep)
}

// sourceTarballSidecar builds the optional informational sidecar the
// apid-side handler records on the build row. Repo + ref are recorded
// verbatim; the build pipeline does NOT use them to fetch upstream.
// DeployedBy is the auto-captured `git config user.name` (or ""
// when unset); the CLI's --deployed-by flag in cmdDeployTarball
// overrides this when present.
func sourceTarballSidecar(owner, repo, sha, deployedBy string) api.SourceTarballDeployRequest {
	// owner + repo are already lowercased by parseGitRemoteURL.
	return api.SourceTarballDeployRequest{
		Repo:       strings.Join([]string{owner, repo}, "/"),
		Ref:        sha,
		DeployedBy: deployedBy,
	}
}

// mustOpen opens path for reading and exits the process on error.
// Used only for tarballs the CLI just produced — no customer file
// is touched here. Routes through openCustomerFile so the tripwire
// at cmd/gregale/lint_tripwires_test.go (no bare os.Open in CLI)
// stays green; the Lstat discipline is overkill but harmless for
// a CLI-produced temp file written seconds earlier with a fresh
// random id.
func mustOpen(path string) *os.File {
	f, err := openCustomerFile(path)
	if err != nil {
		printErr("Could not open packed tarball", err)
		os.Exit(1)
	}
	return f
}
