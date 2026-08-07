package main

import (
	"net/http"
	"os"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
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

// TestDefaultPublicControlAddr_ADR070 pins the loopback control
// listener default at :9092 per ADR-070 (Tier A7 edge split). The
// legacy gatewayd daemon binds :9090 on the same node; a default
// drift here would silently collide and crash-loop both daemons on
// a non-systemd bring-up.
func TestDefaultPublicControlAddr_ADR070(t *testing.T) {
	if defaultPublicControlAddr != "127.0.0.1:9092" {
		t.Errorf("defaultPublicControlAddr = %q, want 127.0.0.1:9092 (ADR-070)", defaultPublicControlAddr)
	}
}

// TestBuildServers_PinsMaxHeaderBytes asserts both servers expose
// MaxHeaderBytes = api.DefaultMaxHeaderBytes. A future stdlib default
// change cannot widen the attack surface on this listener.
func TestBuildServers_PinsMaxHeaderBytes(t *testing.T) {
	pub, ctrl := buildServers("127.0.0.1:8080", "127.0.0.1:9092", http.NotFoundHandler(), http.NewServeMux())
	if pub.MaxHeaderBytes != api.DefaultMaxHeaderBytes {
		t.Errorf("public MaxHeaderBytes = %d, want %d", pub.MaxHeaderBytes, api.DefaultMaxHeaderBytes)
	}
	if ctrl.MaxHeaderBytes != api.DefaultMaxHeaderBytes {
		t.Errorf("control MaxHeaderBytes = %d, want %d", ctrl.MaxHeaderBytes, api.DefaultMaxHeaderBytes)
	}
}
