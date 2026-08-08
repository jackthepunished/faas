package deploycontroller

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/releasebundle"
)

func TestImportLegacyBinCreatesVerifiedRelease(t *testing.T) {
	source := t.TempDir()
	releases := t.TempDir()
	for _, name := range []string{"apid", "imaged", "migrate"} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(source, "imaged.pre-hotfix"), []byte("backup"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest, err := ImportLegacyBin(source, releases, "legacy-import", "legacy-commit", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(releases, "legacy-import")
	if err := releasebundle.Verify(root, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "bin", "imaged.pre-hotfix")); !os.IsNotExist(err) {
		t.Fatalf("backup binary was imported: %v", err)
	}
	if _, err := os.Stat(filepath.Join(source, "imaged")); err != nil {
		t.Fatalf("source binary changed or disappeared: %v", err)
	}
}

func TestImportLegacyBinRefusesExistingDestination(t *testing.T) {
	source := t.TempDir()
	releases := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "apid"), []byte("apid"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(releases, "existing"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportLegacyBin(source, releases, "existing", "commit", time.Now().UTC()); err == nil {
		t.Fatal("ImportLegacyBin succeeded for existing destination")
	}
}

// TestImportedLegacyReleaseIsRollbackBaseline pins the clean-host
// first-release flow: on a host with no immutable release yet, the
// operator imports the legacy /opt/faas/bin state via ImportLegacyBin
// and the imported release becomes the verified previous release the
// controller can roll back to on the first deploy failure (the
// "no previous release for rollback" failure mode seen on the EX44
// box when the first immutable deploy failed without a baseline).
func TestImportedLegacyReleaseIsRollbackBaseline(t *testing.T) {
	legacyBin := t.TempDir()
	releases := t.TempDir()
	for _, name := range []string{"apid", "imaged", "schedd", "vmmd", "migrate", "deployctl"} {
		if err := os.WriteFile(filepath.Join(legacyBin, name), []byte(name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	imported, err := ImportLegacyBin(legacyBin, releases, "legacy-baseline", "legacy-commit", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if imported.ReleaseID != "legacy-baseline" {
		t.Fatalf("imported release id = %q", imported.ReleaseID)
	}
	current := filepath.Join(releases, "current")
	if err := os.Symlink(filepath.Join(releases, "legacy-baseline"), current); err != nil {
		t.Fatal(err)
	}
	// A first immutable candidate deployed after the import sees the
	// imported baseline as its verified previous release.
	makeDryRunRelease(t, releases, "candidate")
	report, err := DryRun(Config{ReleasesRoot: releases, CurrentPath: current}, "candidate")
	if err != nil {
		t.Fatal(err)
	}
	if !report.HasPreviousRelease {
		t.Fatalf("report = %#v, want imported legacy baseline to count as previous release", report)
	}
	if report.CurrentRelease != "legacy-baseline" {
		t.Fatalf("current release = %q, want legacy-baseline", report.CurrentRelease)
	}
}
