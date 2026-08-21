package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/manifest"
	"github.com/onebox-faas/faas/pkg/releaseinstall"
)

func TestInstallViaTarball_RetainsVerifiedTriple(t *testing.T) {
	gitSHA := "0123456789abcdef0123456789abcdef01234567"
	buildRoot := t.TempDir()
	binDir := releaseinstall.BinDir(buildRoot, gitSHA)
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range manifest.SortedHostKeys() {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte("binary-"+name), 0o755); err != nil {
			t.Fatalf("stage %s: %v", name, err)
		}
	}

	tb, err := releaseinstall.BuildTarball(buildRoot, gitSHA, "sha256:"+strings.Repeat("a", 64), time.Now())
	if err != nil {
		t.Fatalf("BuildTarball: %v", err)
	}
	stageDir := t.TempDir()
	tarballPath := filepath.Join(stageDir, releaseTarballName)
	if err := os.WriteFile(tarballPath, tb.Packed, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stageDir, releaseSigName), []byte("signature"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stageDir, releaseSBOMName), []byte(`{"spdxVersion":"SPDX-2.3"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	installRoot := t.TempDir()
	verifier := &releaseinstall.FixtureCosignVerifier{Identity: "test-oidc-identity"}
	if err := installViaTarballWithVerifier(installRoot, gitSHA, tarballPath, verifier); err != nil {
		t.Fatalf("installViaTarballWithVerifier: %v", err)
	}
	if verifier.CallCount != 1 {
		t.Fatalf("cosign calls = %d, want 1", verifier.CallCount)
	}
	gotSHA, err := releaseinstall.CurrentGitSHA(installRoot)
	if err != nil {
		t.Fatalf("CurrentGitSHA: %v", err)
	}
	if gotSHA != gitSHA {
		t.Fatalf("current release = %q, want %q", gotSHA, gitSHA)
	}
	for _, name := range []string{releaseTarballName, releaseSigName, releaseSBOMName, releaseinstall.ManifestName} {
		if _, err := os.Stat(filepath.Join(releaseinstall.BundleRoot(installRoot, gitSHA), name)); err != nil {
			t.Fatalf("installed %s: %v", name, err)
		}
	}
	installedManifest, err := releaseinstall.Read(installRoot, gitSHA)
	if err != nil {
		t.Fatalf("read installed manifest: %v", err)
	}
	if installedManifest.Signature != "test-oidc-identity" {
		t.Fatalf("installed signature = %q, want test identity", installedManifest.Signature)
	}
}
