// Package imaged — cp_test.go: unit tests for the ADR-053
// `cp -a` wrapper used by the parent-ref staging path. imaged
// runs as User=faas-imaged (non-root) and cannot loopback-mount
// the parent ext4 itself; vmmd does it. imaged then `cp -a`
// <mp>/. <staging> to materialise the parent tree.
//
// These tests stay in-process and don't need a real loop
// device (the production mount lives in vmmd, tested separately
// at pkg/vmmdmount + pkg/vmmdgrpc). They cover the empty
// argument rejection, the `/.` idiom enforcement, and the
// happy-path copy that BuildBaseFromStaging relies on.
package imaged

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCopyTree_HappyPath — `cp -a src/. dst` mirrors the
// contents of src into dst, preserving a regular file's mode
// bits. BuildBaseFromStaging mkfs-es the staging dir after
// ApplyLayerGz applies the runtime delta on top, so the
// staging dir must reflect the parent's tree verbatim.
func TestCopyTree_HappyPath(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "file"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Mkdir(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "nested"), []byte("world"), 0o600); err != nil {
		t.Fatalf("WriteFile nested: %v", err)
	}

	if err := CopyTree(context.Background(), src+"/.", dst); err != nil {
		t.Fatalf("CopyTree: %v", err)
	}

	// Top-level file must round-trip.
	gotBytes, err := os.ReadFile(filepath.Join(dst, "file"))
	if err != nil {
		t.Fatalf("ReadFile dst/file: %v", err)
	}
	if string(gotBytes) != "hello" {
		t.Errorf("dst/file = %q, want %q", gotBytes, "hello")
	}
	// Nested file must round-trip too (regression guard: cp -a
	// must recurse, not flatten).
	gotNested, err := os.ReadFile(filepath.Join(dst, "sub", "nested"))
	if err != nil {
		t.Fatalf("ReadFile dst/sub/nested: %v", err)
	}
	if string(gotNested) != "world" {
		t.Errorf("dst/sub/nested = %q, want %q", gotNested, "world")
	}
}

// TestCopyTree_RejectsEmptyArgs — empty src or dst is a
// programming error and fails loud before exec. The
// `cp -a <empty> <empty>` invocation would be a silent
// no-op otherwise.
func TestCopyTree_RejectsEmptyArgs(t *testing.T) {
	if err := CopyTree(context.Background(), "", "/tmp/dst"); err == nil {
		t.Error("empty src should error")
	}
	if err := CopyTree(context.Background(), "/tmp/.", ""); err == nil {
		t.Error("empty dst should error")
	}
}

// TestCopyTree_RequiresSlashDotSuffix — the `/` `.` idiom is
// what makes `cp -a <mp>/. <staging>` produce a staging dir
// that mirrors the parent ext4's root layout, not nest under
// <mp-basename>/. A caller forgetting the suffix would
// produce a base ext4 with a single `/<mp-basename>/` root
// and fail every cold-boot; better to reject loud here.
func TestCopyTree_RequiresSlashDotSuffix(t *testing.T) {
	err := CopyTree(context.Background(), "/tmp/src", "/tmp/dst")
	if err == nil {
		t.Fatal("missing /. suffix should error")
	}
	if !strings.Contains(err.Error(), `"/."`) {
		t.Errorf("error %q must name the required suffix", err.Error())
	}
}