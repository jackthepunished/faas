package main

import (
	"net/http"
	"os"
	"testing"
	"time"

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

// TestBuildServers_PublicListenerPinsKnobs — the customer-facing
// edge installs the canonical customer-facing knob set (ADR-122
// post-merge audit, issue #995 closure): RHT=10s + RT=60s + WT=300s
// + IT=120s (matches apid's customer-facing listener at
// cmd/apid/main.go:452 via APIDIdleTimeoutSecondsDefault=120) +
// MHB=1 MiB. The pre-amendment listener had IdleTimeout=0 (unlimited
// keep-alive); this test pins the new value so a regression is loud.
func TestBuildServers_PublicListenerPinsKnobs(t *testing.T) {
	pub, _ := buildServers("127.0.0.1:8080", "127.0.0.1:9092", http.NotFoundHandler(), http.NewServeMux())
	if pub.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("public RHT = %v, want 10s", pub.ReadHeaderTimeout)
	}
	if pub.ReadTimeout != 60*time.Second {
		t.Errorf("public RT = %v, want 60s", pub.ReadTimeout)
	}
	if pub.WriteTimeout != 300*time.Second {
		t.Errorf("public WT = %v, want 300s", pub.WriteTimeout)
	}
	if pub.IdleTimeout != 120*time.Second {
		t.Errorf("public IT = %v, want 120s (matches apid customer-facing listener)", pub.IdleTimeout)
	}
}

// TestBuildServers_ControlMuxAdoptsMetricsVariant — the loopback
// :9092 control mux installs the canonical metrics variant (ADR-122):
// RHT=10s + RT=10s + WT=10s + IT=60s + MHB=1 MiB. The pre-amendment
// listener had only RHT=5s + MHB; the four missing knobs are the
// audit's Site 2 closure.
func TestBuildServers_ControlMuxAdoptsMetricsVariant(t *testing.T) {
	_, ctrl := buildServers("127.0.0.1:8080", "127.0.0.1:9092", http.NotFoundHandler(), http.NewServeMux())
	if ctrl.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("control RHT = %v, want 10s", ctrl.ReadHeaderTimeout)
	}
	if got, want := ctrl.ReadTimeout, time.Duration(api.MetricsReadTimeoutSecondsDefault)*time.Second; got != want {
		t.Errorf("control RT = %v, want %v", got, want)
	}
	if got, want := ctrl.WriteTimeout, time.Duration(api.MetricsWriteTimeoutSecondsDefault)*time.Second; got != want {
		t.Errorf("control WT = %v, want %v", got, want)
	}
	if got, want := ctrl.IdleTimeout, time.Duration(api.MetricsIdleTimeoutSecondsDefault)*time.Second; got != want {
		t.Errorf("control IT = %v, want %v", got, want)
	}
	if ctrl.MaxHeaderBytes != api.DefaultMaxHeaderBytes {
		t.Errorf("control MHB = %d, want %d", ctrl.MaxHeaderBytes, api.DefaultMaxHeaderBytes)
	}
}
