// Install-side helpers: atomic symlink flip, current-release lookup,
// per-node UPSERT coordination. The "current" symlink is the standard
// deploy convention — /opt/faas/current points to
// /opt/faas/releases/<git-sha>/ — matching pkg/deploycontroller.Config
// (cmd/deployctl/main.go:266/307).
package releaseinstall

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CurrentSymlink is the name of the "current" symlink, sibling of
// the releases directory. Mirrors deploycontroller.Config.CurrentPath
// ("/opt/faas/current" — NOT /opt/faas/releases/current).
//
// The plan/pr-3 recon confirmed this layout: the symlink lives at
// /opt/faas/current, not inside the releases directory.
func CurrentSymlink(releasesRoot string) string {
	return filepath.Join(filepath.Dir(releasesRoot), "current")
}

// InstallResult tells the caller whether the operation was a
// first-time install (AppliedAt was set) or a re-install (the
// release was already applied — the operation was a no-op stamp).
type InstallResult struct {
	GitSHA       string
	FirstApplied bool
}

// CurrentGitSHA reads the "/opt/faas/current" symlink and returns
// the git_sha it points to (the directory under releasesRoot).
// Returns ("", nil) if the symlink doesn't exist (fresh box).
// Returns ("", err) if the symlink exists but is invalid (broken
// target, non-symlink file, etc.).
func CurrentGitSHA(releasesRoot string) (string, error) {
	link := CurrentSymlink(releasesRoot)
	info, err := os.Lstat(link)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("releaseinstall: lstat %s: %w", link, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "", fmt.Errorf("releaseinstall: %s is not a symlink", link)
	}
	target, err := os.Readlink(link)
	if err != nil {
		return "", fmt.Errorf("releaseinstall: readlink %s: %w", link, err)
	}
	// Target is "<git-sha>" or "./<git-sha>" or the absolute path.
	// We extract the basename relative to releasesRoot.
	gitSHA, err := filepath.Rel(releasesRoot, filepath.Join(releasesRoot, target))
	if err != nil {
		return "", fmt.Errorf("releaseinstall: rel %s: %w", target, err)
	}
	// filepath.Rel returns ".." if target is outside releasesRoot;
	// reject that as a misconfigured symlink.
	if gitSHA == ".." || strings.HasPrefix(gitSHA, "../") {
		return "", fmt.Errorf("releaseinstall: symlink target %q is outside releasesRoot", target)
	}
	if gitSHA == "." {
		return "", fmt.Errorf("releaseinstall: symlink target %q points to releasesRoot itself", target)
	}
	return gitSHA, nil
}

// AtomicFlip replaces the "current" symlink so it points at
// <git-sha>. The flip is atomic on POSIX filesystems via os.Rename
// on a staging symlink: write <releasesRoot/.current.tmp> → symlink
// → os.Rename to <current>.
//
// Idempotent: if the symlink already points at <git-sha>, no-op.
// If it points elsewhere, the staging symlink is created and renamed
// over the existing one — POSIX guarantees the swap is atomic.
//
// The release root and the current symlink MUST be on the same
// filesystem (os.Rename is local-only). The bootstrap layout
// guarantees this — both live under /opt/faas.
func AtomicFlip(releasesRoot, gitSHA string) error {
	if gitSHA == "" {
		return errors.New("releaseinstall: empty git_sha")
	}
	if !validGitSHA(gitSHA) {
		return fmt.Errorf("releaseinstall: git_sha %q is not a 40-char lowercase hex", gitSHA)
	}

	// Verify the target release directory exists before flipping.
	target := BundleRoot(releasesRoot, gitSHA)
	if info, err := os.Stat(target); err != nil {
		return fmt.Errorf("releaseinstall: target %s: %w", target, err)
	} else if !info.IsDir() {
		return fmt.Errorf("releaseinstall: target %s is not a directory", target)
	}

	// Idempotency: if the symlink already points at gitSHA, no-op.
	current, err := CurrentGitSHA(releasesRoot)
	if err != nil {
		return fmt.Errorf("releaseinstall: read current: %w", err)
	}
	if current == gitSHA {
		return nil
	}

	link := CurrentSymlink(releasesRoot)
	staging := filepath.Join(releasesRoot, ".current.tmp")
	if err := os.Symlink(gitSHA, staging); err != nil {
		// If the staging symlink already exists from a prior
		// failed flip, remove it and retry once.
		_ = os.Remove(staging)
		if err := os.Symlink(gitSHA, staging); err != nil {
			return fmt.Errorf("releaseinstall: create staging symlink: %w", err)
		}
	}
	if err := os.Rename(staging, link); err != nil {
		_ = os.Remove(staging)
		return fmt.Errorf("releaseinstall: rename staging to %s: %w", link, err)
	}
	return nil
}

// SetCurrentRelease is the legacy /opt/faas/releases/current variant —
// kept for the dryrun audit's path-enumeration check. Most callers
// should use AtomicFlip instead. The symlink lives at
// <releasesRoot>/current (e.g. /opt/faas/releases/current), not at
// <releasesRoot-parent>/current.
//
// Deprecated: use AtomicFlip with the canonical sibling-of-releases
// /opt/faas/current path. Retained for any PR-3.5 path of the
// dryrun legacy checks.
func SetCurrentRelease(releasesRoot, gitSHA string) error {
	link := filepath.Join(releasesRoot, "current")
	target := BundleRoot(releasesRoot, gitSHA)
	if info, err := os.Stat(target); err != nil {
		return fmt.Errorf("releaseinstall: target %s: %w", target, err)
	} else if !info.IsDir() {
		return fmt.Errorf("releaseinstall: target %s is not a directory", target)
	}
	current, err := os.Readlink(link)
	if err == nil && current == gitSHA {
		return nil
	}
	staging := filepath.Join(releasesRoot, ".current.tmp")
	if err := os.Symlink(gitSHA, staging); err != nil {
		_ = os.Remove(staging)
		if err := os.Symlink(gitSHA, staging); err != nil {
			return fmt.Errorf("releaseinstall: create staging symlink: %w", err)
		}
	}
	if err := os.Rename(staging, link); err != nil {
		_ = os.Remove(staging)
		return fmt.Errorf("releaseinstall: rename staging to %s: %w", link, err)
	}
	return nil
}
