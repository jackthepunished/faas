// Mega-PR-A (issue #911 / ADR-110 PR-1): pin the FAAS_NODE_NAME
// env overlay onto both the new builderd Config.NodeName field
// and the legacy BuilderNodeID (ADR-038 default-local sentinel
// override). The overlay must:
//   - win over a TOML node_name (operator doesn't edit TOML on every box add)
//   - override the BuilderNodeID "default-local" sentinel (the
//     default value a fresh install writes, before any env is set)
//   - leave a non-default BuilderNodeID set via TOML untouched
//     (operator intent wins for explicit choices)
//
// Mirrors the per-daemon config_node_name_env_test.go siblings;
// split out so the legacy BuilderNodeID cross-field interaction
// is named in the test, not buried in the overlay logic.
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_NodeNameEnvOverlay(t *testing.T) {
	tomlPath := filepath.Join(t.TempDir(), "builderd.toml")
	if err := os.WriteFile(tomlPath, []byte(`node_name = "from-toml"
builder_node_id = "from-toml-id"`), 0o600); err != nil {
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
	if cfg.BuilderNodeID != "from-toml-id" {
		t.Errorf("BuilderNodeID (non-default TOML kept) = %q, want from-toml-id", cfg.BuilderNodeID)
	}

	// Env overlays the legacy default-local sentinel too.
	t.Setenv("FAAS_NODE_NAME", "from-env-fsn-1")
	cfg, err = LoadConfig(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.NodeName != "from-env-fsn-1" {
		t.Errorf("default NodeName = %q, want from-env-fsn-1", cfg.NodeName)
	}
	if cfg.BuilderNodeID != "from-env-fsn-1" {
		t.Errorf("default BuilderNodeID = %q, want from-env-fsn-1 (default-local sentinel override)", cfg.BuilderNodeID)
	}

	// Empty env keeps the TOML value (no silent override).
	t.Setenv("FAAS_NODE_NAME", "")
	cfg, err = LoadConfig(tomlPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.NodeName != "from-toml" {
		t.Errorf("NodeName (empty env) = %q, want from-toml (TOML untouched)", cfg.NodeName)
	}
}
