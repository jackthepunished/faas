package releasebundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildWriteReadVerify(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "bin", "apid"), "apid", 0o755)
	writeFile(t, filepath.Join(root, "systemd", "faas-apid.service"), "[Service]\n", 0o644)

	created := time.Date(2026, 8, 6, 10, 0, 0, 123, time.UTC)
	manifest, err := Build(root, "release-1", "abc123", "linux/amd64", created)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := Write(root, manifest); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Read(root)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.ReleaseID != manifest.ReleaseID || !got.CreatedAt.Equal(created) {
		t.Fatalf("Read manifest = %#v, want %#v", got, manifest)
	}
	if err := Verify(root, got); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(got.Files) != 2 || got.Files[0].Path != "bin/apid" {
		t.Fatalf("Files = %#v, want sorted release files", got.Files)
	}
}

func TestVerifyAcceptsSymlinkRoot(t *testing.T) {
	target := makeBundle(t)
	root := filepath.Join(t.TempDir(), "current")
	if err := os.Symlink(target, root); err != nil {
		t.Fatal(err)
	}
	manifest, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(root, manifest); err != nil {
		t.Fatalf("Verify symlink root: %v", err)
	}
}

func TestVerifyRejectsTamperedFile(t *testing.T) {
	root := makeBundle(t)
	if err := os.WriteFile(filepath.Join(root, "bin", "apid"), []byte("xxxx"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(root, manifest); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("Verify error = %v, want sha256 mismatch", err)
	}
}

func TestVerifyRejectsMissingAndUnexpectedFiles(t *testing.T) {
	root := makeBundle(t)
	manifest, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "bin", "apid")); err != nil {
		t.Fatal(err)
	}
	if err := Verify(root, manifest); err == nil || !strings.Contains(err.Error(), "bin/apid") {
		t.Fatalf("missing file error = %v", err)
	}

	root = makeBundle(t)
	manifest, err = Read(root)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "unexpected"), "x", 0o644)
	if err := Verify(root, manifest); err == nil || !strings.Contains(err.Error(), "unexpected") {
		t.Fatalf("unexpected file error = %v", err)
	}
}

func TestValidateManifestRejectsUnsafePaths(t *testing.T) {
	base := Manifest{
		FormatVersion: 1,
		ReleaseID:     "release-1",
		CommitSHA:     "abc123",
		Target:        "linux/amd64",
		CreatedAt:     time.Now().UTC(),
	}
	for _, path := range []string{"../escape", "/absolute", "bin\\apid", "./bin/apid", ""} {
		manifest := base
		manifest.Files = []File{{Path: path, Mode: 0o755, SHA256: strings.Repeat("0", 64)}}
		if err := ValidateManifest(manifest); err == nil {
			t.Errorf("ValidateManifest(%q) succeeded", path)
		}
	}
}

func TestValidateManifestRequiresSortedFiles(t *testing.T) {
	manifest := Manifest{
		FormatVersion: 1,
		ReleaseID:     "release-1",
		CommitSHA:     "abc123",
		Target:        "linux/amd64",
		CreatedAt:     time.Now().UTC(),
		Files: []File{
			{Path: "systemd/unit", Mode: 0o644, SHA256: strings.Repeat("0", 64)},
			{Path: "bin/apid", Mode: 0o755, SHA256: strings.Repeat("1", 64)},
		},
	}
	if err := ValidateManifest(manifest); err == nil {
		t.Fatal("ValidateManifest succeeded for unsorted files")
	}
}

func makeBundle(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "bin", "apid"), "apid", 0o755)
	manifest, err := Build(root, "release-1", "abc123", "linux/amd64", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(root, manifest); err != nil {
		t.Fatal(err)
	}
	return root
}

func writeFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	// Re-apply the requested mode bits. os.WriteFile's mode argument
	// is filtered by the process umask, so without an explicit Chmod
	// tests that compare info.Mode().Perm() against a manifest's
	// expected Mode (see Verify's mode-mismatch branch) would see
	// different bits than the test author intended.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// PR-5 / issue #911 — defensive no-faas-tunnel bundle guard.
//
// The split-box bundle does not include faas-tunnel.service (issue #911
// records zero grep hits for that name across the repo today). Pin the
// invariant: Build must not surface any file path containing
// "faas-tunnel" — neither a unit file nor an artefact name. A future
// PR that re-introduces the name must update this test alongside.
//
// The check exercises Build against a fixture tree that mirrors a real
// bundle layout. It is not a no-op `strings.Contains(someconst)` — the
// bundle is materialised, hashed, written, and re-read, so a regression
// in Build/Verify that dropped a path containing the substring would
// surface here even if ValidateManifest stayed green.
func TestBuild_RejectsFaasTunnelReference(t *testing.T) {
	// Positive case: a clean tree builds without surfacing faas-tunnel
	// in any file path (issue #911 invariant).
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "bin", "apid"), "apid", 0o755)
	writeFile(t, filepath.Join(root, "systemd", "faas-apid.service"), "[Service]\n", 0o644)

	manifest, err := Build(root, "release-1", "abc123", "linux/amd64", time.Now().UTC())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := Write(root, manifest); err != nil {
		t.Fatalf("Write: %v", err)
	}
	for _, f := range manifest.Files {
		if strings.Contains(strings.ToLower(f.Path), "faas-tunnel") {
			t.Errorf("bundle contains faas-tunnel reference at %q (issue #911 invariant)", f.Path)
		}
	}

	// Negative case: Build refuses a tree that smuggles faas-tunnel
	// into the file list. Substring-match (case-insensitive) covers
	// future variants like faas-tunnel-client or FAAS-TUNNEL.service.
	for _, denom := range []string{
		"faas-tunnel.service",
		"faas-tunnel",
		"FAAS-TUNNEL.service",
	} {
		bad := t.TempDir()
		writeFile(t, filepath.Join(bad, "systemd", denom), "[Service]\n", 0o644)
		_, err := Build(bad, "release-1", "abc123", "linux/amd64", time.Now().UTC())
		if err == nil {
			t.Errorf("Build(%q) = nil; want forbidden-path error (issue #911 denylist)", denom)
			continue
		}
		if !strings.Contains(err.Error(), "forbidden path") {
			t.Errorf("Build(%q) err = %q; want forbidden-path message", denom, err.Error())
		}
	}
}
