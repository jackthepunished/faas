package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCleanupBaseScratchRemovesOnlyControllerTempFiles(t *testing.T) {
	root := t.TempDir()
	remove := filepath.Join(root, "faas-base-mkfs-old.ext4")
	keepFile := filepath.Join(root, "customer.ext4")
	keepDir := filepath.Join(root, "faas-base-mkfs-active")
	for _, path := range []string{remove, keepFile} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(keepDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := cleanupBaseScratch(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(remove); !os.IsNotExist(err) {
		t.Fatalf("controller temp file still exists: %v", err)
	}
	for _, path := range []string{keepFile, keepDir} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("protected path %s changed: %v", path, err)
		}
	}
}
