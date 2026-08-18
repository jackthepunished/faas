// Mega-PR-A (issue #911 / ADR-110 PR-1): pin the FAAS_NODE_NAME
// env overlay onto gatewayd-internal's Config.NodeName. See
// cmd/apid/config_node_name_env_test.go for the rationale.
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_NodeNameEnvOverlay(t *testing.T) {
	tomlPath := filepath.Join(t.TempDir(), "gatewayd.toml")
	if err := os.WriteFile(tomlPath, []byte(`node_name = "from-toml"`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAAS_NODE_NAME", "from-env-fsn-1")
	cfg, err := LoadConfig(tomlPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.NodeName != "from-env-fsn-1" {
		t.Errorf("NodeName (env overlay) = %q, want from-env-fsn-1", cfg.NodeName)
	}

	t.Setenv("FAAS_NODE_NAME", "")
	cfg, err = LoadConfig(tomlPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.NodeName != "from-toml" {
		t.Errorf("NodeName (empty env) = %q, want from-toml (TOML untouched)", cfg.NodeName)
	}

	t.Setenv("FAAS_NODE_NAME", "")
	cfg, err = LoadConfig(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.NodeName != "" {
		t.Errorf("default NodeName = %q, want empty (single-box dev)", cfg.NodeName)
	}
}
