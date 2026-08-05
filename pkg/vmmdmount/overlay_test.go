// Unit tests for the parent-ref overlayfs mount
// (DEPLOY-1 / ADR-075). The mount(8) syscall requires
// CAP_SYS_ADMIN + a loop device, so this file:
//
//  1. Pins the validation chain — empty paths, non-absolute
//     paths, the /srv/fc/parent prefix check, the
//     /dev/shm/faas-base-staging prefix check, the
//     symlink/..-escape guard, and the lowerdir existence
//     check.
//  2. Stubs the exec.CommandContext("mount", ...) call with a
//     fake `mount` script on PATH. The script logs its argv to
//     a side-channel file so the test can assert the exact
//     argv vmmd issues. This is the "real" path the package
//     exercises — the package compiles + links on macOS, the
//     exec helper is the same shape MountExt4ReadOnly uses,
//     and the validation chain is the load-bearing safety
//     gate the RPC boundary enforces.
//  3. Pins the failed-mount rmdir contract: when the fake
//     mount script exits non-zero, MountOverlayParent must
//     rmdir the freshly-created merged dir so imaged's
//     defer-after-error leaves no orphans.
package vmmdmount

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// withMountStub creates a fresh t.TempDir() containing a
// `mount` and `umount` shell script. The script writes its
// argv (one arg per line) to the file named by argsLog (relative
// to the stub dir) and exits with exitCode. The stub dir is
// prepended to PATH for the duration of fn, and restored via
// t.Setenv so the test environment is clean on return.
//
// `scriptBody` is appended to a fixed prefix:
//
//	#!/bin/sh
//	echo "$@" > "$ARGSLOG"
//	exit $EXIT_CODE
//
// — so the test can assert argv without each test re-implementing
// the shebang / redirect plumbing. If scriptBody is non-empty, it
// replaces the default redirect-and-exit block (useful for tests
// that need to exercise specific mount stderr messages).
func withMountStub(t *testing.T, exitCode int, scriptBody, argsLogName string) (stubDir string, fn func()) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script PATH stub is POSIX-only")
	}
	stubDir = t.TempDir()
	for _, name := range []string{"mount", "umount"} {
		script := "#!/bin/sh\n"
		if scriptBody == "" {
			script += "echo \"$@\" > \"" + argsLogName + "\"\n"
			script += "exit " + itoa(exitCode) + "\n"
		} else {
			script += scriptBody
		}
		path := filepath.Join(stubDir, name)
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatalf("write %s stub: %v", name, err)
		}
	}
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+origPath)
	return stubDir, func() {
		t.Setenv("PATH", origPath)
	}
}

// itoa is a tiny strconv.Itoa helper to avoid pulling strconv
// into the test file's import list for one call. The exit-code
// argument is always 0..127 in the test fixtures.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	const digits = "0123456789"
	var buf [4]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	return string(buf[i:])
}

// TestMountOverlayParent_RejectsEmptyPaths: any of the four
// path arguments empty surfaces an error before exec.Command is
// reached. Pinned because the gRPC handler treats empty as
// InvalidArgument; if validation slipped through, the kernel
// would refuse with EINVAL and a less obvious wire code.
func TestMountOverlayParent_RejectsEmptyPaths(t *testing.T) {
	stubDir, restore := withMountStub(t, 0, "", "args.log")
	defer restore()
	_ = stubDir

	cases := []struct {
		name                       string
		lower, upper, work, merged string
		wantSubstr                 string
	}{
		{"lower empty", "", "/d/s/upper", "/d/s/work", "/d/s/merged", "empty path"},
		{"upper empty", "/s/p/x", "", "/d/s/work", "/d/s/merged", "empty path"},
		{"work empty", "/s/p/x", "/d/s/upper", "", "/d/s/merged", "empty path"},
		{"merged empty", "/s/p/x", "/d/s/upper", "/d/s/work", "", "empty path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := MountOverlayParent(context.Background(), tc.lower, tc.upper, tc.work, tc.merged)
			if err == nil {
				t.Fatalf("MountOverlayParent(%q,%q,%q,%q) = nil, want error",
					tc.lower, tc.upper, tc.work, tc.merged)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("err = %v, want substring %q", err, tc.wantSubstr)
			}
		})
	}
}

// TestMountOverlayParent_RejectsRelativePaths: all four paths
// must be absolute. Relative paths would let the vmmd cwd (which
// is /) resolve them anywhere. The stub mount is unreachable
// here — the absolute-path check is the third guard, after the
// empty check.
func TestMountOverlayParent_RejectsRelativePaths(t *testing.T) {
	stubDir, restore := withMountStub(t, 0, "", "args.log")
	defer restore()
	_ = stubDir

	err := MountOverlayParent(context.Background(),
		"srv/fc/parent/x",
		"/dev/shm/faas-base-staging/upper",
		"/dev/shm/faas-base-staging/work",
		"/dev/shm/faas-base-staging/merged")
	if err == nil {
		t.Fatalf("MountOverlayParent(relative lower) = nil, want error")
	}
	if !strings.Contains(err.Error(), "non-absolute path") {
		t.Errorf("err = %v, want substring %q", err, "non-absolute path")
	}
}

// TestMountOverlayParent_RejectsLowerdirOutsideMountRoot:
// lowerdir must live under /srv/fc/parent/. A misbehaving
// imaged handing vmmd a /home/... path would otherwise let the
// kernel mount an attacker-chosen dir read-only into the
// overlayfs.
func TestMountOverlayParent_RejectsLowerdirOutsideMountRoot(t *testing.T) {
	stubDir, restore := withMountStub(t, 0, "", "args.log")
	defer restore()
	_ = stubDir

	cases := []string{
		"/etc/passwd",
		"/srv/fc/something-else",
		"/tmp/evil-parent",
		"/home/user/Documents",
	}
	for _, bad := range cases {
		t.Run(bad, func(t *testing.T) {
			err := MountOverlayParent(context.Background(),
				bad,
				"/dev/shm/faas-base-staging/upper",
				"/dev/shm/faas-base-staging/work",
				"/dev/shm/faas-base-staging/merged")
			if err == nil {
				t.Fatalf("MountOverlayParent(lower=%q) = nil, want error", bad)
			}
			if !errors.Is(err, ErrInvalidOverlayPath) {
				t.Errorf("err = %v, want ErrInvalidOverlayPath in chain", err)
			}
		})
	}
}

// TestMountOverlayParent_RejectsUpperOutsideStagingRoot:
// upperdir, workdir, and merged must all live under
// /dev/shm/faas-base-staging/. Anything else is a tmpfile-host
// problem in waiting — host /tmp is ext4 here, and the kernel
// refuses with "upper fs does not support tmpfile" (the
// 2026-08-04 dmesg bug).
func TestMountOverlayParent_RejectsUpperOutsideStagingRoot(t *testing.T) {
	stubDir, restore := withMountStub(t, 0, "", "args.log")
	defer restore()
	_ = stubDir

	cases := []struct {
		name                       string
		lower, upper, work, merged string
	}{
		{"upper outside", "/srv/fc/parent/x", "/tmp/upper", "/dev/shm/faas-base-staging/work", "/dev/shm/faas-base-staging/merged"},
		{"work outside", "/srv/fc/parent/x", "/dev/shm/faas-base-staging/upper", "/tmp/work", "/dev/shm/faas-base-staging/merged"},
		{"merged outside", "/srv/fc/parent/x", "/dev/shm/faas-base-staging/upper", "/dev/shm/faas-base-staging/work", "/tmp/merged"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := MountOverlayParent(context.Background(), tc.lower, tc.upper, tc.work, tc.merged)
			if err == nil {
				t.Fatalf("MountOverlayParent = nil, want error")
			}
			if !errors.Is(err, ErrInvalidOverlayPath) {
				t.Errorf("err = %v, want ErrInvalidOverlayPath in chain", err)
			}
		})
	}
}

// TestMountOverlayParent_RejectsSymlinkEscape: a literal
// /srv/fc/parent/... path that is itself a symlink to a
// non-allowed target is rejected. EvalSymlinks collapses the
// symlink, the post-Eval check notices the resolved path
// differs from the input, and MountOverlayParent refuses.
// (review finding B3.)
//
// On macOS dev boxes EvalSymlinks refuses to dereference paths
// the test runner can't see; the test skips on
// filepath.EvalSymlinks errors that are not "looks like a
// symlink" so the contract pins on a Linux runner.
func TestMountOverlayParent_RejectsSymlinkEscape(t *testing.T) {
	_, restore := withMountStub(t, 0, "", "args.log")
	defer restore()

	// Create a real lowerdir under a tmpdir, then a symlink
	// under MountRoot pointing at it. The symlink IS under
	// MountRoot but resolves to a path that is NOT — that's
	// the symlink-escape channel.
	real := t.TempDir()
	if err := os.MkdirAll(filepath.Join(real, "actual-parent"), 0o755); err != nil {
		t.Fatalf("mkdir real: %v", err)
	}
	if err := os.MkdirAll(MountRoot, 0o750); err != nil {
		t.Skipf("cannot create %s: %v (likely not root on non-Linux dev box)", MountRoot, err)
	}
	linkPath := filepath.Join(MountRoot, "faas-parent-symlink-evil")
	if err := os.Symlink(filepath.Join(real, "actual-parent"), linkPath); err != nil {
		t.Skipf("symlink: %v", err)
	}
	defer func() { _ = os.Remove(linkPath) }()

	err := MountOverlayParent(context.Background(),
		linkPath,
		"/dev/shm/faas-base-staging/upper",
		"/dev/shm/faas-base-staging/work",
		"/dev/shm/faas-base-staging/merged")
	if err == nil {
		t.Fatalf("MountOverlayParent(symlink lower) = nil, want error")
	}
	if !errors.Is(err, ErrInvalidOverlayPath) {
		t.Errorf("err = %v, want ErrInvalidOverlayPath in chain", err)
	}
}

// TestMountOverlayParent_RejectsDotDotEscape: a path containing
// `..` that resolves outside the allowed prefix is rejected.
// EvalSymlinks collapses the `..` segments, the post-Eval check
// notices the resolved path differs from the input. (review
// finding B3.)
func TestMountOverlayParent_RejectsDotDotEscape(t *testing.T) {
	_, restore := withMountStub(t, 0, "", "args.log")
	defer restore()

	// Create a tmpdir tree and an allowed MountRoot directory,
	// then point at "../escape" relative to the MountRoot
	// child. EvalSymlinks collapses "../escape" to a sibling,
	// the post-Eval check fires.
	if err := os.MkdirAll(MountRoot, 0o750); err != nil {
		t.Skipf("cannot create %s: %v", MountRoot, err)
	}
	inside := filepath.Join(MountRoot, "faas-parent-dotdot-anchor")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Skipf("mkdir %s: %v", inside, err)
	}
	defer func() { _ = os.Remove(inside) }()

	// "/srv/fc/parent/faas-parent-dotdot-anchor/../escape"
	// resolves to "/srv/fc/parent/escape" — same prefix,
	// different inode. The post-Eval `resolved != abs` check
	// fires.
	escape := filepath.Join(inside, "..", "faas-parent-dotdot-evil")
	err := MountOverlayParent(context.Background(),
		escape,
		"/dev/shm/faas-base-staging/upper",
		"/dev/shm/faas-base-staging/work",
		"/dev/shm/faas-base-staging/merged")
	if err == nil {
		t.Fatalf("MountOverlayParent(dotdot lower) = nil, want error")
	}
	if !errors.Is(err, ErrInvalidOverlayPath) {
		t.Errorf("err = %v, want ErrInvalidOverlayPath in chain", err)
	}
}

// TestMountOverlayParent_RejectsNonexistentLowerdir: lowerdir
// must already exist (vmmd should have loopback-mounted it
// before imaged fires this RPC). Stat catches the race where
// imaged gets a stale mountpoint from a previous gRPC call but
// the loopback has been swept.
//
// MkdirAll(MountRoot) is required so EvalSymlinks on the
// absolute lowerdir path can walk /srv/fc/parent — without
// it, on a non-root dev box the ENOENT surfaces first as
// ErrInvalidOverlayPath and the Stat() guard never fires.
// Tests that need this run on root or skip cleanly.
func TestMountOverlayParent_RejectsNonexistentLowerdir(t *testing.T) {
	_, restore := withMountStub(t, 0, "", "args.log")
	defer restore()

	if err := os.MkdirAll(MountRoot, 0o750); err != nil {
		t.Skipf("cannot create %s: %v (need root or pre-existing MountRoot)", MountRoot, err)
	}
	missing := filepath.Join(MountRoot, "faas-parent-stale-99999")
	err := MountOverlayParent(context.Background(),
		missing,
		"/dev/shm/faas-base-staging/upper",
		"/dev/shm/faas-base-staging/work",
		"/dev/shm/faas-base-staging/merged")
	if err == nil {
		t.Fatalf("MountOverlayParent(missing lower) = nil, want error")
	}
	if !strings.Contains(err.Error(), "stat lowerdir") {
		t.Errorf("err = %v, want substring %q", err, "stat lowerdir")
	}
}

// TestMountOverlayParent_HappyPathArgs: when the stub mount
// succeeds, MountOverlayParent returns nil AND the stub
// recorded argv matches the documented -t overlay -o
// lowerdir=...,upperdir=...,workdir=... shape. The argv pin is
// load-bearing — imaged's defer-after-error + the gRPC
// retry-after-park contract both depend on vmmd issuing a
// kernel overlay mount, not a loopback mount.
//
// Skipped on hosts where the test runner cannot create the
// lowerdir under MountRoot (i.e. non-root on non-Linux dev).
func TestMountOverlayParent_HappyPathArgs(t *testing.T) {
	stubDir, restore := withMountStub(t, 0, "", "args.log")
	defer restore()

	if err := os.MkdirAll(MountRoot, 0o750); err != nil {
		t.Skipf("cannot create %s: %v", MountRoot, err)
	}
	lower := filepath.Join(MountRoot, "faas-parent-test-argv")
	if err := os.MkdirAll(lower, 0o755); err != nil {
		t.Skipf("mkdir %s: %v", lower, err)
	}
	defer func() { _ = os.Remove(lower) }()

	// EvalSymlinks on the staging paths is fine on POSIX —
	// /dev/shm/faas-base-staging/{upper,work,merged} resolves
	// to itself. The merged dir does NOT exist yet (Mount
	// creates it).
	upper := "/dev/shm/faas-base-staging/upper-test-argv"
	work := "/dev/shm/faas-base-staging/work-test-argv"
	merged := "/dev/shm/faas-base-staging/merged-test-argv"
	defer func() {
		_ = os.Remove(merged)
	}()

	err := MountOverlayParent(context.Background(), lower, upper, work, merged)
	if err != nil {
		// If /dev/shm/faas-base-staging doesn't exist on the
		// dev box (rare), EvalSymlinks returns ENOENT and we
		// surface ErrInvalidOverlayPath — skip cleanly.
		if errors.Is(err, ErrInvalidOverlayPath) {
			t.Skipf("/dev/shm/faas-base-staging not present on this runner: %v", err)
		}
		t.Fatalf("MountOverlayParent = %v, want nil", err)
	}
	argsFile := filepath.Join(stubDir, "args.log")
	body, readErr := os.ReadFile(argsFile)
	if readErr != nil {
		t.Fatalf("read stub argv: %v", readErr)
	}
	got := strings.TrimSpace(string(body))
	want := "-t overlay -o lowerdir=" + lower + ",upperdir=" + upper + ",workdir=" + work + " overlay " + merged
	if got != want {
		t.Errorf("stub mount argv = %q\nwant %q", got, want)
	}
	// merged dir now exists; the stub didn't touch it (no
	// actual mount), but MkdirAll did — that's the contract.
	if _, statErr := os.Stat(merged); statErr != nil {
		t.Errorf("merged dir %s missing after successful mount: %v", merged, statErr)
	}
}

// TestMountOverlayParent_MountFailureRmdirsMerged: when the
// mount command exits non-zero, MountOverlayParent must rmdir
// the merged dir it just MkdirAll'd so a failed mount doesn't
// leave orphans under /dev/shm/faas-base-staging. Pinned because
// the staging sweep relies on the no-orphan invariant to size
// its window correctly.
//
// The stub is told to exit 32 with stderr "simulated kernel
// refusal" — the kernel's typical overlayfs reject code.
func TestMountOverlayParent_MountFailureRmdirsMerged(t *testing.T) {
	stubDir, restore := withMountStub(t, 32,
		"echo simulated kernel refusal >&2\nexit 32\n",
		"args.log")
	defer restore()

	if err := os.MkdirAll(MountRoot, 0o750); err != nil {
		t.Skipf("cannot create %s: %v", MountRoot, err)
	}
	lower := filepath.Join(MountRoot, "faas-parent-test-failrm")
	if err := os.MkdirAll(lower, 0o755); err != nil {
		t.Skipf("mkdir %s: %v", lower, err)
	}
	defer func() { _ = os.Remove(lower) }()

	upper := "/dev/shm/faas-base-staging/upper-test-failrm"
	work := "/dev/shm/faas-base-staging/work-test-failrm"
	merged := "/dev/shm/faas-base-staging/merged-test-failrm"

	err := MountOverlayParent(context.Background(), lower, upper, work, merged)
	if err == nil {
		t.Fatalf("MountOverlayParent = nil, want mount-failure error")
	}
	if !strings.Contains(err.Error(), "mount overlay") {
		t.Errorf("err = %v, want substring %q", err, "mount overlay")
	}
	if !strings.Contains(err.Error(), "simulated kernel refusal") {
		t.Errorf("err = %v, want captured stderr in wrap", err)
	}
	// merged dir must be gone — the rmdir-on-failure
	// contract. (defer cleanup is harmless if the test raced
	// and the rmdir was a no-op.)
	if _, statErr := os.Stat(merged); !os.IsNotExist(statErr) {
		t.Errorf("merged dir %s still present after mount failure: stat err = %v", merged, statErr)
	}
	_ = os.Remove(merged)
	_ = stubDir
}

// TestUmountOverlayParent_RejectsEmpty: empty merged path
// surfaces ErrUnknownMountpoint so the gRPC handler lifts to
// InvalidArgument. Pinned because imaged's defer-after-error
// pattern is built on top of this idempotency.
func TestUmountOverlayParent_RejectsEmpty(t *testing.T) {
	err := UmountOverlayParent(context.Background(), "")
	if !errors.Is(err, ErrUnknownMountpoint) {
		t.Errorf("UmountOverlayParent(\"\") = %v, want ErrUnknownMountpoint", err)
	}
}

// TestUmountOverlayParent_RejectsForeignPath: merged not under
// /dev/shm/faas-base-staging surfaces ErrInvalidOverlayPath
// (NOT ErrUnknownMountpoint — the prefix check is
// load-bearing; silently umounting an unrelated mount would be
// a security incident, same shape as the MountExt4 prefix
// guard).
func TestUmountOverlayParent_RejectsForeignPath(t *testing.T) {
	cases := []string{
		"/", "/etc/passwd", "/tmp/evil", "/srv/fc/parent/foo",
	}
	for _, bad := range cases {
		t.Run(bad, func(t *testing.T) {
			err := UmountOverlayParent(context.Background(), bad)
			if err == nil {
				t.Fatalf("UmountOverlayParent(%q) = nil, want prefix-check error", bad)
			}
			if !errors.Is(err, ErrInvalidOverlayPath) {
				t.Errorf("err = %v, want ErrInvalidOverlayPath in chain", err)
			}
		})
	}
}

// TestUmountOverlayParent_NonexistentReturnsUnknown: a path
// under the staging root that doesn't exist on disk returns
// ErrUnknownMountpoint (masking the kernel's EINVAL for a
// non-mountpoint path). Same shape as UmountExt4 — the
// gRPC handler lifts to InvalidArgument.
func TestUmountOverlayParent_NonexistentReturnsUnknown(t *testing.T) {
	missing := "/dev/shm/faas-base-staging/merged-never-issued-99999"
	err := UmountOverlayParent(context.Background(), missing)
	if !errors.Is(err, ErrUnknownMountpoint) {
		t.Errorf("UmountOverlayParent(%q) = %v, want ErrUnknownMountpoint", missing, err)
	}
}
