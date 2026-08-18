//go:build metal && linux

package builderd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
)

func TestReadBuildDonePrefersGuestExitCode(t *testing.T) {
	dir := t.TempDir()
	want := api.BuildDone{
		SchemaVersion: 1,
		BuildID:       "build-1",
		ExitCode:      0,
		OCIImagePath:  "/build/out/image.tar",
		LogTail:       "build complete",
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "build-done.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	got, ok := readBuildDone(dir)
	if !ok {
		t.Fatal("readBuildDone returned !ok")
	}
	if got.ExitCode != 0 || got.OCIImagePath != want.OCIImagePath || got.LogTail != want.LogTail {
		t.Fatalf("readBuildDone = %#v, want %#v", got, want)
	}
}
