package renderer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishAtomic_WritesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.toml")
	body := []byte("socket_path = \"/run/faas/x.sock\"\n")

	digest, changed, err := publishAtomic(path, body, 0o644)
	if err != nil {
		t.Fatalf("publishAtomic: %v", err)
	}
	if !changed {
		t.Errorf("changed = false, want true on first publish")
	}
	if digest == "" {
		t.Errorf("digest is empty")
	}

	// File landed with the right content + mode.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("written content = %q, want %q", got, body)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0o644", info.Mode().Perm())
	}

	// No temp leftovers.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir has %d entries; expected only the published file: %v", len(entries), names)
	}
}

func TestPublishAtomic_IdempotentOnMatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.toml")
	body := []byte("foo = \"bar\"\n")

	if _, _, err := publishAtomic(path, body, 0o644); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	digest, changed, err := publishAtomic(path, body, 0o644)
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if changed {
		t.Errorf("changed = true on second publish; want false (idempotent)")
	}
	if digest == "" {
		t.Errorf("digest is empty on no-op path")
	}
}

func TestPublishAtomic_DetectsContentChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.toml")
	body1 := []byte("foo = \"bar\"\n")
	body2 := []byte("foo = \"baz\"\n")

	if _, _, err := publishAtomic(path, body1, 0o644); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	_, changed, err := publishAtomic(path, body2, 0o644)
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if !changed {
		t.Errorf("changed = false on content-different publish; want true")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(body2) {
		t.Errorf("content = %q, want %q", got, body2)
	}
}

func TestPublishAtomic_RejectsExistingDir(t *testing.T) {
	dir := t.TempDir()
	// Create a directory at the target path.
	path := filepath.Join(dir, "out.toml")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	_, _, err := publishAtomic(path, []byte("body"), 0o644)
	if err == nil {
		t.Errorf("publishAtomic over existing dir = nil err, want error")
	}
	// The function may take the read-existing branch (yielding
	// "is a directory") or the rename branch (yielding "rename").
	// Both are valid signals that the publish was rejected.
	if err != nil && !strings.Contains(err.Error(), "rename") &&
		!strings.Contains(err.Error(), "is a directory") {
		t.Errorf("err = %v, want a rename- or directory-related error", err)
	}
}

func TestPublishAtomic_CleansUpTempOnError(t *testing.T) {
	// Force a write failure by passing a body that would later
	// fail at sync. Hard to trigger cleanly without mocking; the
	// rename-fails-on-dir path above already exercises the cleanup
	// indirectly. This test pins the contract: deterministic
	// cleanup if the publish aborts.
	dir := t.TempDir()
	path := filepath.Join(dir, "out.toml")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, _, err := publishAtomic(path, []byte("body"), 0o644)
	if err == nil {
		t.Fatalf("publish = nil err")
	}
	// Temp files start with `.render-` and end with `.tmp`.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".render-") && strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file leaked: %s", e.Name())
		}
	}
}

func TestPublishCgroupControl_WritesAndMatchesControllerSet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cgroup.subtree_control")
	body := []byte("+cpu\n+memory\n")

	digest, changed, err := publishCgroupControl(path, body)
	if err != nil {
		t.Fatalf("first publishCgroupControl: %v", err)
	}
	if !changed || digest == "" {
		t.Fatalf("first publishCgroupControl = digest %q, changed %v; want digest and changed", digest, changed)
	}

	// cgroup v2 reports enabled controllers without '+' and may format them
	// on one line; both forms represent the same enabled set.
	if err := os.WriteFile(path, []byte("cpu memory\n"), 0o644); err != nil {
		t.Fatalf("rewrite pseudo-file fixture: %v", err)
	}
	digest, changed, err = publishCgroupControl(path, body)
	if err != nil {
		t.Fatalf("idempotent publishCgroupControl: %v", err)
	}
	if changed {
		t.Errorf("idempotent publishCgroupControl changed = true, want false")
	}
	if digest == "" {
		t.Errorf("idempotent digest is empty")
	}
}

func TestHashFileIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data")
	body := []byte("hello, world\n")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	h1, err := hashManifestFile(path)
	if err != nil {
		t.Fatalf("hash 1: %v", err)
	}
	h2, err := hashManifestFile(path)
	if err != nil {
		t.Fatalf("hash 2: %v", err)
	}
	if h1 != h2 {
		t.Errorf("hash drift: %q vs %q", h1, h2)
	}
	if len(h1) != 64 {
		t.Errorf("hash len = %d, want 64 (sha256 hex)", len(h1))
	}
}

func TestInstallCurrentSymlink_Idempotent(t *testing.T) {
	root := t.TempDir()
	releases := filepath.Join(root, "releases")
	if err := os.MkdirAll(filepath.Join(releases, "v1"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	current := filepath.Join(root, "current")

	if err := installCurrentSymlink(current, filepath.Join(releases, "v1")); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if err := installCurrentSymlink(current, filepath.Join(releases, "v1")); err != nil {
		t.Fatalf("second install (idempotent): %v", err)
	}
	target, err := os.Readlink(current)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if !strings.HasSuffix(target, "v1") {
		t.Errorf("target = %q, want suffix v1", target)
	}
}

func TestInstallCurrentSymlink_RecoversFromStaging(t *testing.T) {
	root := t.TempDir()
	releases := filepath.Join(root, "releases")
	if err := os.MkdirAll(filepath.Join(releases, "v1"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	current := filepath.Join(root, "current")
	staging := filepath.Join(root, ".current.tmp")

	// Plant a stale staging symlink.
	if err := os.Symlink("stale", staging); err != nil {
		t.Fatalf("plant staging: %v", err)
	}
	if err := installCurrentSymlink(current, filepath.Join(releases, "v1")); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := os.Lstat(staging); err == nil {
		t.Errorf("staging symlink leaked")
	}
}
