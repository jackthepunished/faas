// Tests for meterd config loading (issue #678 PR-0): defaults,
// NodeName round-trip, partial-cluster rejection on TLS helpers,
// dropping the deprecated GatewayEgressTLS field set.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
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

// TestConfig_MetricsListener_Defaults — when no TOML fields are set, the
// helper returns the api.Metrics*SecondsDefault family. ADR-122
// guarantees every daemon's metrics listener inherits the canonical
// shape from pkg/api/limits.go, so a stray edit to that constant
// family must surface here (the api.* tests guard the constants;
// this test guards the wiring).
func TestConfig_MetricsListener_Defaults(t *testing.T) {
	c := &Config{}
	read, write, idle, mhb := c.MetricsListener()
	if read != time.Duration(api.MetricsReadTimeoutSecondsDefault)*time.Second {
		t.Errorf("read=%v want %v", read, api.MetricsReadTimeoutSecondsDefault)
	}
	if write != time.Duration(api.MetricsWriteTimeoutSecondsDefault)*time.Second {
		t.Errorf("write=%v want %v", write, api.MetricsWriteTimeoutSecondsDefault)
	}
	if idle != time.Duration(api.MetricsIdleTimeoutSecondsDefault)*time.Second {
		t.Errorf("idle=%v want %v", idle, api.MetricsIdleTimeoutSecondsDefault)
	}
	if mhb != api.DefaultMaxHeaderBytes {
		t.Errorf("mhb=%v want %v", mhb, api.DefaultMaxHeaderBytes)
	}
}

// TestConfig_MetricsListener_OverridesRespected — explicit TOML values
// must pass through unchanged (no constant fallback when the field is
// non-zero). This is the operational override path: an operator with a
// slow scraper bumps cfg.MetricsReadTimeout to 30s without losing the
// rest of the canonical shape.
func TestConfig_MetricsListener_OverridesRespected(t *testing.T) {
	c := &Config{
		MetricsReadTimeout:    30 * time.Second,
		MetricsWriteTimeout:   45 * time.Second,
		MetricsIdleTimeout:    120 * time.Second,
		MetricsMaxHeaderBytes: 4 << 20, // 4 MiB
	}
	read, write, idle, mhb := c.MetricsListener()
	if read != 30*time.Second || write != 45*time.Second || idle != 120*time.Second || mhb != int64(4<<20) {
		t.Errorf("override lost: read=%v write=%v idle=%v mhb=%v", read, write, idle, mhb)
	}
}
