package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadJoinFleetFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodes.yaml")
	if err := os.WriteFile(path, []byte("nodes:\n  - node: fsn-3\n    ssh_host: 203.0.113.3\n  - node: fsn-4\n    ssh_host: 203.0.113.4\n    ssh_port: 2222\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := loadJoinFleetFile(path)
	if err != nil {
		t.Fatalf("loadJoinFleetFile: %v", err)
	}
	if len(file.Nodes) != 2 || file.Nodes[1].SSHPort != 2222 {
		t.Fatalf("nodes = %#v", file.Nodes)
	}
}

func TestLoadJoinFleetInputsFromClaim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claim.yaml")
	claim := `api_version: gregale.dev/v1alpha1
kind: ComputeNodeClaim
metadata:
  name: fsn-3
spec:
  ssh:
    host: 203.0.113.27
    user: deploy
    port: 2222
  storage:
    device: /dev/disk/by-id/data
    format: true
`
	if err := os.WriteFile(path, []byte(claim), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := loadJoinFleetInputs("", path)
	if err != nil {
		t.Fatalf("loadJoinFleetInputs: %v", err)
	}
	if len(file.Nodes) != 1 {
		t.Fatalf("nodes = %#v", file.Nodes)
	}
	n := file.Nodes[0]
	if n.Node != "fsn-3" || n.SSHHost != "203.0.113.27" || n.SSHUser != "deploy" || n.SSHPort != 2222 {
		t.Fatalf("connection = %#v", n)
	}
	if n.StorageDevice != "/dev/disk/by-id/data" || !n.FormatStorage {
		t.Fatalf("storage = %#v", n)
	}
}

func TestResolveJoinArtifactsDoesNotOverrideExplicitPaths(t *testing.T) {
	artifactDir := t.TempDir()
	explicit := filepath.Join(t.TempDir(), "custom.tar.gz")
	opts := deployJoinOptions{ArtifactDir: artifactDir, ReleaseTarball: explicit}
	resolveJoinArtifacts(&opts)
	if opts.ReleaseTarball != explicit {
		t.Fatalf("explicit release tarball was replaced: %q", opts.ReleaseTarball)
	}
	if opts.BootstrapBinary != filepath.Join(artifactDir, "gregalectl-linux-amd64") {
		t.Fatalf("bootstrap binary = %q", opts.BootstrapBinary)
	}
	if opts.PKISource != filepath.Join(artifactDir, "pki") {
		t.Fatalf("PKI source = %q", opts.PKISource)
	}
}
