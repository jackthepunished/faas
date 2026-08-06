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
