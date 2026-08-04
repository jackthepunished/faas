package main

import (
	"os"
	"testing"
)

// TestEnvOr_EmptyFallback pins the envOr semantics (empty env
// falls back to def). The default for FAAS_PUBLIC_LISTEN_ADDR is
// 127.0.0.1:8080 in plain-HTTP mode (was :443 in TLS mode).
func TestEnvOr_EmptyFallback(t *testing.T) {
	t.Setenv("FAAS_PUBLIC_LISTEN_ADDR", "")
	got := envOr("FAAS_PUBLIC_LISTEN_ADDR", "127.0.0.1:8080")
	if got != "127.0.0.1:8080" {
		t.Errorf("envOr empty = %q, want 127.0.0.1:8080", got)
	}
	t.Setenv("FAAS_PUBLIC_LISTEN_ADDR", "127.0.0.1:18443")
	got = envOr("FAAS_PUBLIC_LISTEN_ADDR", "127.0.0.1:8080")
	if got != "127.0.0.1:18443" {
		t.Errorf("envOr set = %q, want 127.0.0.1:18443", got)
	}
}

// TestHstsEnabledFromEnv_LookupEnv pins the os.LookupEnv path
// (per the FAAS_APID_METRICS_ADDR empty=skip precedent). An
// explicit empty value must be distinguishable from unset.
func TestHstsEnabledFromEnv_LookupEnv(t *testing.T) {
	t.Setenv("FAAS_HSTS_ENABLED", "")
	if v := hstsEnabledFromEnv("FAAS_HSTS_ENABLED"); v != "" {
		t.Errorf("hstsEnabledFromEnv explicit-empty = %q, want empty string", v)
	}
	t.Setenv("FAAS_HSTS_ENABLED", "true")
	if v := hstsEnabledFromEnv("FAAS_HSTS_ENABLED"); v != "true" {
		t.Errorf("hstsEnabledFromEnv set = %q, want true", v)
	}
	// unset
	if err := os.Unsetenv("FAAS_HSTS_ENABLED"); err != nil {
		t.Fatalf("unsetenv: %v", err)
	}
	if v := hstsEnabledFromEnv("FAAS_HSTS_ENABLED"); v != "" {
		t.Errorf("hstsEnabledFromEnv unset = %q, want empty string", v)
	}
}
