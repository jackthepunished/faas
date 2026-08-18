// Pin the [ratelimit] TOML knob onto gatewayd-internal's Config
// (ADR-104 amendment 5, issue #881 Phase 4 C2). Mirrors the
// node_name_env test shape — TOML round-trip + default + missing
// file shape — so a future config refactor that drops or renames
// the field surfaces here, not at the daemon boot.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig_RateLimitModeDefault(t *testing.T) {
	// Missing file → RateLimit.Mode is "local" (the back-compat
	// default). Single-box dev MUST NOT require a TOML entry to
	// start the daemon.
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.RateLimit.Mode != "local" {
		t.Errorf("default RateLimit.Mode = %q, want local (back-compat default)", cfg.RateLimit.Mode)
	}
}

func TestLoadConfig_RateLimitModeCentral(t *testing.T) {
	// TOML sets mode = "central"; LoadConfig must round-trip the
	// value unchanged. Wire-time validation (rejecting unknown
	// values) lives in run.go's startup gate — the config package
	// only knows about the on-disk shape.
	path := filepath.Join(t.TempDir(), "gatewayd.toml")
	if err := os.WriteFile(path, []byte("[ratelimit]\nmode = \"central\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.RateLimit.Mode != "central" {
		t.Errorf("RateLimit.Mode = %q, want central (TOML round-trip)", cfg.RateLimit.Mode)
	}
}

func TestLoadConfig_RateLimitModeUnknownAccepted(t *testing.T) {
	// The config layer accepts any string; run.go validates the
	// closed vocabulary and rejects unknown values at boot. This
	// pins the shape — a future move that adds a strict enum
	// validator here would surface here, not at startup.
	path := filepath.Join(t.TempDir(), "gatewayd.toml")
	if err := os.WriteFile(path, []byte("[ratelimit]\nmode = \"nonsense\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !strings.Contains(cfg.RateLimit.Mode, "nonsense") {
		t.Errorf("RateLimit.Mode = %q, want nonsense preserved verbatim (boot gate validates, not config)", cfg.RateLimit.Mode)
	}
}
