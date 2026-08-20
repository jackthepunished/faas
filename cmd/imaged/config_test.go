// Tests for imaged config loading (ADR-122 follow-on to imaged
// env-only path): defaults, missing file, parse errors, MetricsAddr
// env overlay, and the canonical MetricsListener shape.
package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
)

func TestLoadConfig_MissingFileReturnsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.toml")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("missing file: %v", err)
	}
	if cfg.MetricsAddr != "127.0.0.1:9102" {
		t.Errorf("MetricsAddr = %q, want default", cfg.MetricsAddr)
	}
}

func TestLoadConfig_OverridesFromTOML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "imaged.toml")
	body := `
metrics_addr = "127.0.0.1:9103"
metrics_read_timeout = "30s"
metrics_write_timeout = "45s"
metrics_idle_timeout = "120s"
metrics_max_header_bytes = 4194304
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.MetricsAddr != "127.0.0.1:9103" {
		t.Errorf("MetricsAddr = %q", cfg.MetricsAddr)
	}
	if cfg.MetricsReadTimeout != 30*time.Second {
		t.Errorf("MetricsReadTimeout = %v", cfg.MetricsReadTimeout)
	}
	if cfg.MetricsWriteTimeout != 45*time.Second {
		t.Errorf("MetricsWriteTimeout = %v", cfg.MetricsWriteTimeout)
	}
	if cfg.MetricsIdleTimeout != 120*time.Second {
		t.Errorf("MetricsIdleTimeout = %v", cfg.MetricsIdleTimeout)
	}
	if cfg.MetricsMaxHeaderBytes != 4<<20 {
		t.Errorf("MetricsMaxHeaderBytes = %d", cfg.MetricsMaxHeaderBytes)
	}
}

// TestConfig_GetMetricsAddr_EnvOverlay — pre-existing operator knob
// (FAAS_IMAGED_METRICS_ADDR) must win over the TOML field. Empty env
// keeps the TOML value. The shape mirrors cmd/apid/config.go's
// GetMetricsAddr.
func TestConfig_GetMetricsAddr_EnvOverlay(t *testing.T) {
	cfg := &Config{MetricsAddr: "127.0.0.1:9102"}
	env := func(string) string { return "" } // empty: TOML wins
	if got := cfg.GetMetricsAddr(env); got != "127.0.0.1:9102" {
		t.Errorf("empty env: got %q want %q", got, cfg.MetricsAddr)
	}
	env = func(string) string { return "127.0.0.1:9999" }
	if got := cfg.GetMetricsAddr(env); got != "127.0.0.1:9999" {
		t.Errorf("env set: got %q want env value", got)
	}
}

// TestConfig_MetricsListener_Defaults / OverridesRespected — ADR-122
// canonical shape (mirrors cmd/meterd/config_test.go).
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

func TestConfig_MetricsListener_OverridesRespected(t *testing.T) {
	c := &Config{
		MetricsReadTimeout:    30 * time.Second,
		MetricsWriteTimeout:   45 * time.Second,
		MetricsIdleTimeout:    120 * time.Second,
		MetricsMaxHeaderBytes: 4 << 20,
	}
	read, write, idle, mhb := c.MetricsListener()
	if read != 30*time.Second || write != 45*time.Second || idle != 120*time.Second || mhb != int64(4<<20) {
		t.Errorf("override lost: read=%v write=%v idle=%v mhb=%v", read, write, idle, mhb)
	}
}
