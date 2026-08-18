package imaged

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandArtifactReplicatorPassesLayerAndSignatureKeys(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "args")
	helper := filepath.Join(dir, "helper.sh")
	script := "#!/bin/sh\nprintf '%s\\n%s\\n' \"$1\" \"$2\" > \"$REPLICATOR_TEST_OUTPUT\"\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	r := CommandArtifactReplicator{
		Path:     helper,
		ExtraEnv: []string{"REPLICATOR_TEST_OUTPUT=" + out},
	}
	if err := r.Replicate(context.Background(), "apps/demo/01234567-89ab-cdef-0123-456789abcdef.ext4"); err != nil {
		t.Fatalf("Replicate: %v", err)
	}

	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want := "apps/demo/01234567-89ab-cdef-0123-456789abcdef.ext4\n" +
		"sigs/apps/demo/01234567-89ab-cdef-0123-456789abcdef.ext4.sig\n"
	if string(b) != want {
		t.Fatalf("helper args = %q, want %q", b, want)
	}
}

func TestCommandArtifactReplicatorIncludesBoundedHelperOutput(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "helper.sh")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf '%*s' 3000 x >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	err := (CommandArtifactReplicator{Path: helper}).Replicate(context.Background(), "apps/demo/01234567-89ab-cdef-0123-456789abcdef.ext4")
	if err == nil {
		t.Fatal("Replicate succeeded; want helper failure")
	}
	if got := len(err.Error()); got > 2200 {
		t.Fatalf("error length = %d, want bounded helper output", got)
	}
	if !strings.Contains(err.Error(), "helper") {
		t.Fatalf("error = %q, want helper context", err)
	}
}
