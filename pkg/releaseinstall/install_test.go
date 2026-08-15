// Install tests for pkg/releaseinstall. Covers:
//   - CurrentSymlink lives sibling of releases dir, not inside
//   - CurrentGitSHA returns "" when symlink absent
//   - AtomicFlip creates the symlink atomically and is idempotent
//   - AtomicFlip refuses malformed git_sha
//   - AtomicFlip refuses missing target directory
//   - SetCurrentRelease writes the legacy inside-releasesRoot path
package releaseinstall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCurrentSymlinkLivesSiblingOfReleases(t *testing.T) {
	releasesRoot := "/opt/faas/releases"
	got := CurrentSymlink(releasesRoot)
	want := "/opt/faas/current"
	if got != want {
		t.Errorf("CurrentSymlink(%q) = %q, want %q", releasesRoot, got, want)
	}
}

func setupReleaseDir(t *testing.T, root, gitSHA string) {
	t.Helper()
	bin := filepath.Join(root, gitSHA, BinDirName)
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bin, "vmmd"), []byte("fake"), 0o644); err != nil {
		t.Fatalf("write vmmd: %v", err)
	}
}

func TestCurrentGitSHA_Absent(t *testing.T) {
	dir := t.TempDir()
	releasesRoot := filepath.Join(dir, "releases")
	if err := os.MkdirAll(releasesRoot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	gitSHA, err := CurrentGitSHA(releasesRoot)
	if err != nil {
		t.Errorf("CurrentGitSHA = err, want nil: %v", err)
	}
	if gitSHA != "" {
		t.Errorf("CurrentGitSHA = %q, want empty", gitSHA)
	}
}

func TestAtomicFlip_CreatesSymlink(t *testing.T) {
	dir := t.TempDir()
	releasesRoot := filepath.Join(dir, "releases")
	if err := os.MkdirAll(releasesRoot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	gitSHA := "0123456789abcdef0123456789abcdef01234567"
	setupReleaseDir(t, releasesRoot, gitSHA)

	if err := AtomicFlip(releasesRoot, gitSHA); err != nil {
		t.Fatalf("AtomicFlip: %v", err)
	}

	link := filepath.Join(dir, "current")
	// Confirm symlink exists, points at the right target.
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat %s: %v", link, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("%s is not a symlink", link)
	}
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != gitSHA {
		t.Errorf("symlink target = %q, want %q", target, gitSHA)
	}
	// CurrentGitSHA now round-trips.
	got, err := CurrentGitSHA(releasesRoot)
	if err != nil {
		t.Fatalf("CurrentGitSHA: %v", err)
	}
	if got != gitSHA {
		t.Errorf("CurrentGitSHA = %q, want %q", got, gitSHA)
	}
}

func TestAtomicFlip_Idempotent(t *testing.T) {
	dir := t.TempDir()
	releasesRoot := filepath.Join(dir, "releases")
	if err := os.MkdirAll(releasesRoot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	gitSHA := "0123456789abcdef0123456789abcdef01234567"
	setupReleaseDir(t, releasesRoot, gitSHA)

	if err := AtomicFlip(releasesRoot, gitSHA); err != nil {
		t.Fatalf("AtomicFlip 1: %v", err)
	}
	if err := AtomicFlip(releasesRoot, gitSHA); err != nil {
		t.Fatalf("AtomicFlip 2 (idempotent): %v", err)
	}
	// Staging symlink must NOT be left behind.
	if _, err := os.Lstat(filepath.Join(releasesRoot, ".current.tmp")); err == nil {
		t.Errorf("staging symlink leaked after AtomicFlip")
	}
}

func TestAtomicFlip_FlipsAcrossReleases(t *testing.T) {
	dir := t.TempDir()
	releasesRoot := filepath.Join(dir, "releases")
	if err := os.MkdirAll(releasesRoot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sha1 := "0123456789abcdef0123456789abcdef01234567"
	sha2 := "89abcdef0123456789abcdef0123456789abcdef"
	setupReleaseDir(t, releasesRoot, sha1)
	setupReleaseDir(t, releasesRoot, sha2)

	if err := AtomicFlip(releasesRoot, sha1); err != nil {
		t.Fatalf("AtomicFlip sha1: %v", err)
	}
	if err := AtomicFlip(releasesRoot, sha2); err != nil {
		t.Fatalf("AtomicFlip sha2: %v", err)
	}
	got, err := CurrentGitSHA(releasesRoot)
	if err != nil {
		t.Fatalf("CurrentGitSHA: %v", err)
	}
	if got != sha2 {
		t.Errorf("CurrentGitSHA = %q, want %q", got, sha2)
	}
}

func TestAtomicFlip_RejectsMalformedGitSHA(t *testing.T) {
	dir := t.TempDir()
	releasesRoot := filepath.Join(dir, "releases")
	if err := os.MkdirAll(releasesRoot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, bad := range []string{"", "abc", "0123456789ABCDEF0123456789ABCDEF01234567"} {
		if err := AtomicFlip(releasesRoot, bad); err == nil {
			t.Errorf("AtomicFlip(%q) = nil err, want error", bad)
		}
	}
}

func TestAtomicFlip_RejectsMissingTarget(t *testing.T) {
	dir := t.TempDir()
	releasesRoot := filepath.Join(dir, "releases")
	if err := os.MkdirAll(releasesRoot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	gitSHA := "0123456789abcdef0123456789abcdef01234567"
	if err := AtomicFlip(releasesRoot, gitSHA); err == nil {
		t.Errorf("AtomicFlip on missing target = nil err, want error")
	}
}

func TestAtomicFlip_RecoversFromStagingSymlink(t *testing.T) {
	// If a prior failed flip left a staging symlink, AtomicFlip
	// must clean it up rather than fail.
	dir := t.TempDir()
	releasesRoot := filepath.Join(dir, "releases")
	if err := os.MkdirAll(releasesRoot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	gitSHA := "0123456789abcdef0123456789abcdef01234567"
	setupReleaseDir(t, releasesRoot, gitSHA)

	// Plant a stale staging symlink pointing somewhere bogus.
	staging := filepath.Join(releasesRoot, ".current.tmp")
	if err := os.Symlink("stale", staging); err != nil {
		t.Fatalf("plant stale staging: %v", err)
	}
	if err := AtomicFlip(releasesRoot, gitSHA); err != nil {
		t.Fatalf("AtomicFlip with stale staging: %v", err)
	}
	if _, err := os.Lstat(staging); err == nil {
		t.Errorf("staging symlink leaked after recovery")
	}
}

func TestSetCurrentRelease_InsideReleasesRoot(t *testing.T) {
	dir := t.TempDir()
	releasesRoot := filepath.Join(dir, "releases")
	if err := os.MkdirAll(releasesRoot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	gitSHA := "0123456789abcdef0123456789abcdef01234567"
	setupReleaseDir(t, releasesRoot, gitSHA)

	if err := SetCurrentRelease(releasesRoot, gitSHA); err != nil {
		t.Fatalf("SetCurrentRelease: %v", err)
	}
	link := filepath.Join(releasesRoot, "current")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink %s: %v", link, err)
	}
	if target != gitSHA {
		t.Errorf("legacy symlink target = %q, want %q", target, gitSHA)
	}
	// Sanity: NOT the sibling-of-releases path.
	if strings.Contains(link, filepath.Dir(releasesRoot)) && !strings.HasSuffix(link, "releases/current") {
		t.Errorf("legacy symlink path %q not inside releasesRoot", link)
	}
}
