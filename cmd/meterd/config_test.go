// Tests for meterd config loading (issue #678 PR-0): defaults,
// NodeName round-trip, partial-cluster rejection on TLS helpers,
// dropping the deprecated GatewayEgressTLS field set.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadConfig_MissingFileReturnsDefaults pins the back-compat
// behaviour: a missing /etc/faas/meterd.toml produces a working
// daemon with the legacy defaults (single-box unix socket, dev
// mode, no TLS). Same shape as cmd/vmmd/config_test.go.
func TestLoadConfig_MissingFileReturnsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.toml")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("missing file: %v", err)
	}
	if cfg.SocketPath != "/run/faas/schedd.sock" {
		t.Errorf("SocketPath = %q, want default", cfg.SocketPath)
	}
	if cfg.EgressSocket == "" {
		t.Errorf("EgressSocket = %q, want non-empty default", cfg.EgressSocket)
	}
	// Issue #678 PR-0: NodeName defaults empty (single-box path).
	if cfg.NodeName != "" {
		t.Errorf("NodeName = %q, want empty (single-box default)", cfg.NodeName)
	}
}

// TestLoadConfig_NodeNameRoundTrip pins the toml round-trip for the
// meterd node_name field. A future refactor that renames the field
// or drops the toml tag would silently break PR-B's wiring.
func TestLoadConfig_NodeNameRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meterd.toml")
	body := `node_name = "fsn-1-meterd"` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.NodeName != "fsn-1-meterd" {
		t.Errorf("NodeName = %q, want %q", cfg.NodeName, "fsn-1-meterd")
	}
}

func TestLoadConfig_BadTOMLErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.toml")
	if err := os.WriteFile(path, []byte("not valid toml === ==="), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("error %q should mention parse failure", err.Error())
	}
}

func TestConfig_LoadScheddTLS(t *testing.T) {
	c := &Config{}
	tls, err := c.LoadScheddTLS()
	if err != nil || tls != nil {
		t.Errorf("all-empty: tls=%v err=%v, want nil", tls, err)
	}

	c.ScheddTLSCertPath = "/some/cert"
	if _, err := c.LoadScheddTLS(); err == nil {
		t.Errorf("partial (cert only): expected error naming missing fields")
	} else if !strings.Contains(err.Error(), "schedd_tls_key_path") || !strings.Contains(err.Error(), "schedd_tls_ca_path") {
		t.Errorf("err = %q, want both schedd_tls_key_path and schedd_tls_ca_path named", err.Error())
	}
}

func TestConfig_LoadEgressTLS(t *testing.T) {
	c := &Config{}
	tls, err := c.LoadEgressTLS()
	if err != nil || tls != nil {
		t.Errorf("all-empty: tls=%v err=%v, want nil", tls, err)
	}

	c.EgressTLSCertPath = "/some/cert"
	if _, err := c.LoadEgressTLS(); err == nil {
		t.Errorf("partial (cert only): expected error naming missing fields")
	} else if !strings.Contains(err.Error(), "egress_tls_key_path") || !strings.Contains(err.Error(), "egress_tls_ca_path") {
		t.Errorf("err = %q, want both egress_tls_key_path and egress_tls_ca_path named", err.Error())
	}
}
