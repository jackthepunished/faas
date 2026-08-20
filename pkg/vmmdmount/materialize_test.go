package vmmdmount

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCopyParentTreeCopiesContentsIntoTarget(t *testing.T) {
	source := t.TempDir()
	target := t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "usr", "bin"), 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	const payload = "parent-file"
	if err := os.WriteFile(filepath.Join(source, "usr", "bin", "node"), []byte(payload), 0o755); err != nil {
		t.Fatalf("write source: %v", err)
	}

	if err := copyParentTree(context.Background(), source, target); err != nil {
		t.Fatalf("copyParentTree: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(target, "usr", "bin", "node"))
	if err != nil {
		t.Fatalf("read copied file: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("copied payload = %q, want %q", got, payload)
	}
	if _, err := os.Stat(filepath.Join(target, filepath.Base(source), "usr", "bin", "node")); !os.IsNotExist(err) {
		t.Fatalf("parent directory was nested under target: err=%v", err)
	}
}
