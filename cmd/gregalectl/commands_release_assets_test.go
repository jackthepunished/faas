package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadCanonicalReleaseAsset_UsesWorkflowSibling(t *testing.T) {
	dir := t.TempDir()
	tarball := filepath.Join(dir, "release.tar.gz")
	want := []byte("workflow bundle")
	if err := os.WriteFile(filepath.Join(dir, releaseSigName), want, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tarball+".cosign.bundle", []byte("legacy bundle"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := readCanonicalReleaseAsset(tarball, releaseSigName)
	if err != nil {
		t.Fatalf("readCanonicalReleaseAsset: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("asset = %q, want %q", got, want)
	}
}

func TestReadCanonicalReleaseAsset_AcceptsLegacySidecar(t *testing.T) {
	dir := t.TempDir()
	tarball := filepath.Join(dir, "release.tar.gz")
	want := []byte("legacy sbom")
	if err := os.WriteFile(tarball+".sbom.json", want, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := readCanonicalReleaseAsset(tarball, releaseSBOMName)
	if err != nil {
		t.Fatalf("readCanonicalReleaseAsset: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("asset = %q, want %q", got, want)
	}
}

func TestReadCanonicalReleaseAsset_ReportsAllCandidates(t *testing.T) {
	tarball := filepath.Join(t.TempDir(), "release.tar.gz")
	_, err := readCanonicalReleaseAsset(tarball, releaseSigName)
	if err == nil {
		t.Fatal("readCanonicalReleaseAsset returned nil error for missing asset")
	}
	if !strings.Contains(err.Error(), filepath.Join(filepath.Dir(tarball), releaseSigName)) ||
		!strings.Contains(err.Error(), tarball+".cosign.bundle") {
		t.Fatalf("error = %q, want both candidate paths", err)
	}
}
