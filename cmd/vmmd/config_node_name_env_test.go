// Mega-PR-A (issue #911 / ADR-110 PR-1): pin the FAAS_NODE_NAME
// env overlay onto vmmd's [compute_node].name. The vmmd ComputeNode
// self-registration (cmd/vmmd/register.go) writes this value into
// compute_nodes.name at startup, so the overlay is the load-bearing
// identity for the multi-box handshake. Mirrors the per-daemon
// config_node_name_env_test.go siblings; coverage split out so
// vmmd's [compute_node] shape (vs the flat NodeName on
// schedd/apid/meterd/githubd/gatewayd-internal/builderd) gets a
// test that names the field correctly.
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_ComputeNodeNodeNameEnvOverlay(t *testing.T) {
	tomlPath := filepath.Join(t.TempDir(), "vmmd.toml")
	if err := os.WriteFile(tomlPath, []byte(`[compute_node]
name = "from-toml"`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAAS_NODE_NAME", "from-env-fsn-1")
	cfg, err := LoadConfig(tomlPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ComputeNode.NodeName != "from-env-fsn-1" {
		t.Errorf("ComputeNode.NodeName (env overlay) = %q, want from-env-fsn-1", cfg.ComputeNode.NodeName)
	}

	t.Setenv("FAAS_NODE_NAME", "")
	cfg, err = LoadConfig(tomlPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ComputeNode.NodeName != "from-toml" {
		t.Errorf("ComputeNode.NodeName (empty env) = %q, want from-toml (TOML untouched)", cfg.ComputeNode.NodeName)
	}

	t.Setenv("FAAS_NODE_NAME", "")
	cfg, err = LoadConfig(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ComputeNode.NodeName != "" {
		t.Errorf("default ComputeNode.NodeName = %q, want empty (short-hostname default)", cfg.ComputeNode.NodeName)
	}
}
