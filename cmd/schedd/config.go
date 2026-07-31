// schedd config — parsed from /etc/faas/schedd.toml. Every field has a working
// default so a missing or partial file still yields a runnable daemon.

package main

import (
	"crypto/tls"
	"fmt"
	"os"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/onebox-faas/faas/pkg/wire"
)

// Config is the on-disk representation of schedd's TOML config.
type Config struct {
	// SocketPath is the unix-domain socket schedd's gRPC server binds when
	// ListenAddr is empty (ADR-018, mode 0660 group `faas`). Defaults to
	// /run/faas/schedd.sock.
	SocketPath string `toml:"socket_path"`

	// ListenAddr is the location-transparent gRPC listen target
	// (issue #95, ADR-025). Accepts unix:///path or tcp://host:port.
	// When empty, falls back to unix://+SocketPath for backwards
	// compatibility. tcp targets require all server TLS paths to be set.
	ListenAddr string `toml:"listen_addr"`

	// VMMDSocket is the vmmd gRPC socket schedd dials when VMMTarget is
	// empty. Defaults to /run/faas/vmmd.sock. (ADR-014)
	VMMDSocket string `toml:"vmmd_socket"`

	// VMMTarget is the location-transparent gRPC dial target for vmmd
	// (issue #95, ADR-025). When non-empty, takes precedence over
	// VMMDSocket and supports the unix|tcp|dns schemes.
	VMMTarget string `toml:"vmmd_target"`

	// VMMTLS* configure the mTLS material schedd uses to dial vmmd
	// (issue #95). All three paths empty => no TLS; all three set =>
	// RequireAndVerifyClientCert. Partial cluster => startup error.
	VMMTLSCertPath string `toml:"vmmd_tls_cert_path"`
	VMMTLSKeyPath  string `toml:"vmmd_tls_key_path"`
	VMMTLSCAPath   string `toml:"vmmd_tls_ca_path"`

	// Server-mTLS material for the gatewayd-facing gRPC surface (issue
	// #95). All three paths empty => no TLS; all three set =>
	// RequireAndVerifyClientCert. Partial cluster => startup error.
	TLSCertPath string `toml:"tls_cert_path"`
	TLSKeyPath  string `toml:"tls_key_path"`
	TLSCAPath   string `toml:"tls_ca_path"`

	// GatewaySynthSocket is the legacy unix-domain socket schedd dials
	// to fire synthetic cron requests through gatewayd (spec §4.4, M7).
	// Mode 0660 group `faas` (ADR-015). Defaults to
	// /run/faas/gatewayd-internal.sock. Deprecated: multi-box schedd
	// uses GatewaySynthTarget (a wire.ParseTarget-style URL). Setting
	// GatewaySynthSocket alone keeps the legacy one-box behaviour;
	// setting GatewaySynthTarget takes precedence.
	GatewaySynthSocket string `toml:"gateway_synth_socket"`

	// GatewaySynthTarget is the wire.ParseTarget-style URL schedd
	// uses to dial gatewayd's internal listener (placement scheduler
	// PR, ADR-025 axis 3, Q8). Accepts unix://|tcp://|dns://.
	// Multi-box operators set this to
	// tcp://<gatewayd-overlay-ip>:9090 (or https://... when the
	// tailnet ACL isn't enough on its own). Empty GatewaySynthTarget
	// falls back to the legacy GatewaySynthSocket for backwards
	// compatibility — existing tests + the e2e harness rely on the
	// legacy field name. The fallback lives in cmd/schedd/main.go
	// so LoadConfig stays a thin TOML-to-struct mapping.
	GatewaySynthTarget string `toml:"gateway_synth_target"`

	// OwnerUser owns the socket file (looked up by name). Defaults to
	// faas-schedd. Only consulted when the resolved listen target is
	// a unix socket.
	OwnerUser string `toml:"owner_user"`

	// MetricsAddr is the optional bind address for /metrics. Empty disables it.
	MetricsAddr string `toml:"metrics_addr"`

	// DBURL is the Postgres DSN; empty falls back to $DATABASE_URL (db.Open).
	DBURL string `toml:"db_url"`

	// RetentionDuration is the §17 retention sweep window (PR #74).
	// STOPPED/FAILED instances are DELETED this long after entering the
	// terminal state. Zero or negative reverts to
	// api.DefaultInstanceRetention (30d). The sweep itself runs at the
	// api.DefaultRetentionInterval cadence (1h) regardless.
	RetentionDuration int64 `toml:"retention_duration_ns"`

	// HeartbeatInterval is the per-node liveness sweep cadence
	// (issue #97 / ADR-025 axis 3, PR #114). Zero or negative reverts
	// to sched.DefaultHeartbeatInterval (30s). Shorter is fine for
	// dev boxes but raises Postgres write traffic — production
	// should leave it at the default unless ops have a reason.
	HeartbeatInterval time.Duration `toml:"heartbeat_interval"`

	// HeartbeatStaleness is the age threshold at which a stale
	// last_heartbeat_at flips active=false (issue #98 / ADR-028
	// acceptance: "Watchdog marks a node active=false after 90s of
	// missed pings"). Zero or negative reverts to
	// sched.DefaultHeartbeatStaleness (90s). The invariant
	// HeartbeatInterval < HeartbeatStaleness prevents a single
	// missed tick from deactivating a healthy node — keep at
	// least 2 × Interval.
	HeartbeatStaleness time.Duration `toml:"heartbeat_staleness"`

	// GatewayMetricsURL is the absolute URL of gatewayd's /metrics
	// endpoint (issue #169 / #172). The schedd scale-up trigger
	// scrapes this URL every cfg.ScaleUpInterval for
	// `gateway_requests_total{app=...}` so it can compute per-app
	// RPS. Empty disables the trigger (Loop.WithScaleUp is not
	// called → the ticker arm never fires). Defaults to
	// http://127.0.0.1:9090/metrics, matching gatewayd's
	// ControlAddr default (cmd/gatewayd/config.go).
	GatewayMetricsURL string `toml:"gateway_metrics_url"`

	// ScaleUpInterval is the per-app reactive scale-up trigger
	// cadence. Zero or negative reverts to
	// api.ScaleUpDecisionIntervalSeconds (1s). 1s is the right
	// balance between "admit Nth instance before the gateway
	// queue builds" and "don't hammer Postgres with a full app
	// list on every tick" — the trigger reads from apps +
	// instances per tick.
	ScaleUpInterval time.Duration `toml:"scaleup_interval"`

	// ReaperAggressive (issue #171) toggles the aggressive-reaper
	// scale-down path. Default ON (true) — schedd parks surplus
	// instances above max(min_instances, desired + 1) on the next
	// 10 s reaper tick when recent-window RPS is below target.
	// Set false via FAAS_REAPER_AGGRESSIVE=false to disable
	// in-place if a regression surfaces; the signal mirror still
	// runs so the metric and the audit row surface for diagnosis.
	// The flag does NOT disable the existing ReapIdle timeout
	// reaper — only the new path.
	ReaperAggressive bool `toml:"reaper_aggressive"`

	// ReaperAggressiveParkCap (issue #171) caps the number of
	// aggressive-path parks per app per 10 s tick. Zero reverts
	// to sched.MaxParksPerTickPerApp (= 8). The cap prevents a
	// single tick from blocking the reaper for `cap × ~150 ms`
	// during a sudden-scale-down storm. The existing
	// ReapIdle / SelectEvictions paths are NOT capped — they
	// already drain at their own cadences.
	ReaperAggressiveParkCap int `toml:"reaper_aggressive_park_cap"`

	// NodeName is the multi-box gate (ADR-056, mirrored from vmmd's
	// [compute_node].name). When set, schedd constructs the
	// handshake-layer NodeVerifier and surfaces a populated
	// compute_nodes snapshot to every mTLS leg on listen. Empty
	// (one-box dev / pre-slice-3 schedd) keeps the verifier off
	// entirely — stdlib chain + RFC 6125 SAN + EKU alone run. The
	// synthetic `default-local` row seeded by migration 00024 is
	// always present, so the verifier, when wired, finds at least
	// one entry to bind against.
	//
	// The field is intentionally not backed by [compute_node] TOML
	// subsection for schedd: schedd is the control-plane trust
	// anchor across every compute node, not a self-registrant.
	// Operators set node_name = "schedd-<box>" through this field
	// and the [compute_nodes] row is provisioned by `faas node
	// register` (out of scope for ADR-056).
	NodeName string `toml:"node_name"`
}

// ResolveListenTarget returns the gRPC target schedd should bind.
// ListenAddr wins when set; otherwise unix://+SocketPath.
func (c *Config) ResolveListenTarget() string {
	if c.ListenAddr != "" {
		return c.ListenAddr
	}
	return "unix://" + c.SocketPath
}

// ResolveVMMTarget returns the gRPC dial target for vmmd. VMMTarget
// wins when set; otherwise unix://+VMMDSocket.
func (c *Config) ResolveVMMTarget() string {
	if c.VMMTarget != "" {
		return c.VMMTarget
	}
	return "unix://" + c.VMMDSocket
}

// LoadServerTLS returns the server's mTLS config when all three TLS
// paths are set, or (nil, nil) when none are set. Partial cluster is
// rejected — wire.LoadServerTLSConfig names the missing fields.
func (c *Config) LoadServerTLS() (*tls.Config, error) {
	return wire.LoadServerTLSConfig(c.TLSCertPath, c.TLSKeyPath, c.TLSCAPath)
}

// LoadServerTLSWithVerifier is the ADR-056 variant of LoadServerTLS.
// Schedd is the control-plane trust anchor, so it wires the
// verifier unconditionally when the multi-box gate is open.
func (c *Config) LoadServerTLSWithVerifier(v wire.NodeVerifier) (*tls.Config, error) {
	return wire.LoadServerTLSConfigWithVerifier(c.TLSCertPath, c.TLSKeyPath, c.TLSCAPath, v)
}

// LoadVMMTLS returns the client mTLS config schedd uses to dial vmmd.
// Empty cluster returns (nil, nil) — single-box default. Partial
// cluster is rejected with the vmmd_tls_* field names (not the
// generic tls_*) so an operator can map the error straight to a TOML
// key.
func (c *Config) LoadVMMTLS() (*tls.Config, error) {
	return wire.LoadClientTLSConfigWithPrefix("vmmd_", c.VMMTLSCertPath, c.VMMTLSKeyPath, c.VMMTLSCAPath)
}

// LoadVMMTLSWithVerifier is the ADR-056 variant of LoadVMMTLS.
// Mirrors the prefix semantics (vmmd_ for error naming).
func (c *Config) LoadVMMTLSWithVerifier(v wire.NodeVerifier) (*tls.Config, error) {
	return wire.LoadClientTLSConfigWithPrefixAndVerifier("vmmd_", c.VMMTLSCertPath, c.VMMTLSKeyPath, c.VMMTLSCAPath, v)
}

// LoadConfig reads a TOML file at path with defaults filled in. A missing file
// is not an error — the defaults produce a working daemon.
func LoadConfig(path string) (*Config, error) {
	c := &Config{
		SocketPath:         "/run/faas/schedd.sock",
		VMMDSocket:         "/run/faas/vmmd.sock",
		GatewaySynthSocket: "/run/faas/gatewayd-internal.sock",
		// GatewaySynthTarget stays empty by default so the fallback
		// in cmd/schedd/main.go (synthTarget == "" → "unix://"+
		// GatewaySynthSocket) owns the default-target resolution.
		// That preserves the one-box path (synthTarget resolves to
		// "unix:///run/faas/gatewayd-internal.sock") AND lets the
		// e2e harness's gateway_synth_socket TOML entry actually
		// override the dial — a previous PR landed a non-empty
		// default here, which silently shadowed the legacy socket
		// and broke the drain goroutine in
		// TestE2E_AsyncInvoke_PostEnqueuesRowAndDrainCompletesIt
		// and TestE2E_QueueSend_DrainLongPoll (e2e harness points
		// gateway_synth_socket at /tmp/.../gatewayd-internal.sock).
		// Multi-box operators set gateway_synth_target TOML or
		// FAAS_GATEWAY_SYNTH_TARGET env to take precedence.
		GatewaySynthTarget: "",
		OwnerUser:          "faas-schedd",
		// Issue #169 / #172: default to gatewayd's loopback
		// control listener. Empty disables the trigger (the
		// loop with WithScaleUp(nil) skips the ticker arm).
		GatewayMetricsURL: "http://127.0.0.1:9090/metrics",
		// issue #171: aggressive reaper defaults to ON. Operators
		// can flip FAAS_REAPER_AGGRESSIVE=false to disable in-place
		// without redeploying.
		ReaperAggressive: true,
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, fmt.Errorf("schedd: read %q: %w", path, err)
	}
	if err := toml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("schedd: parse %q: %w", path, err)
	}
	return c, nil
}
