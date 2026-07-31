// meterd config — parsed from /etc/faas/meterd.toml. Mirrors the schedd
// pattern (cmd/schedd/config.go): every field has a working default so a
// missing or partial file still yields a runnable daemon.

package main

import (
	"crypto/tls"
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
	"github.com/onebox-faas/faas/pkg/meter"
	"github.com/onebox-faas/faas/pkg/wire"
)

// Config is the on-disk representation of meterd's TOML config.
type Config struct {
	// SocketPath is the schedd unix socket meterd dials to call ParkInstance
	// on Free-tier hard stop (slice 4 adds the RPC, ADR-019).
	SocketPath string `toml:"schedd_socket"`
	// DBURL is the Postgres DSN; empty falls back to $DATABASE_URL.
	DBURL string `toml:"db_url"`
	// MetricsAddr is the optional bind address for /metrics. Empty disables it.
	MetricsAddr string `toml:"metrics_addr"`
	// Meter is the pkg/meter timer cadence + behavior block.
	Meter *meter.Config `toml:"meter"`

	// ScheddTLS is the client mTLS material meterd uses to dial schedd
	// (ADR-052 / issue #95 slice 2). All three paths empty => no TLS,
	// single-box default; all three set => mTLS to remote schedd. Partial
	// cluster => startup error naming the missing fields.
	ScheddTLSCertPath string `toml:"schedd_tls_cert_path"`
	ScheddTLSKeyPath  string `toml:"schedd_tls_key_path"`
	ScheddTLSCAPath   string `toml:"schedd_tls_ca_path"`

	// GatewayEgressSocket is the gatewayd egress dial target meterd
	// dials to read tx_bytes (ADR-046). Defaults to
	// /run/faas/gatewayd-egress.sock; multi-box deployments override
	// with tcp:// or dns:// plus the gateway_egress_tls_* cluster.
	GatewayEgressSocket string `toml:"gateway_egress_socket"`

	// GatewayEgressTLSCertPath / Key / CA configure the mTLS material
	// meterd uses to dial gatewayd's egress listener when it lives on a
	// remote compute node (ADR-052). All three empty => no TLS (single-box
	// path uses the unix socket above); partial cluster => startup error.
	// Field names are prefixed with gateway_egress_ so an operator can map
	// the error straight to a TOML key.
	GatewayEgressTLSCertPath string `toml:"gateway_egress_tls_cert_path"`
	GatewayEgressTLSKeyPath  string `toml:"gateway_egress_tls_key_path"`
	GatewayEgressTLSCAPath   string `toml:"gateway_egress_tls_ca_path"`
}

// LoadScheddTLS returns the client mTLS config meterd uses to dial
// schedd. Empty cluster returns (nil, nil); partial cluster is rejected.
func (c *Config) LoadScheddTLS() (*tls.Config, error) {
	return wire.LoadClientTLSConfigWithPrefix("schedd_", c.ScheddTLSCertPath, c.ScheddTLSKeyPath, c.ScheddTLSCAPath)
}

// LoadGatewayEgressTLS returns the client mTLS config meterd uses to
// dial gatewayd's egress listener. Empty cluster returns (nil, nil);
// partial cluster is rejected.
func (c *Config) LoadGatewayEgressTLS() (*tls.Config, error) {
	return wire.LoadClientTLSConfigWithPrefix("gateway_egress_", c.GatewayEgressTLSCertPath, c.GatewayEgressTLSKeyPath, c.GatewayEgressTLSCAPath)
}

// LoadConfig reads a TOML file at path with defaults filled in. A missing
// file is not an error — the defaults produce a working daemon.
func LoadConfig(path string) (*Config, error) {
	c := &Config{
		SocketPath:          "/run/faas/schedd.sock",
		GatewayEgressSocket: "/run/faas/gatewayd-egress.sock",
		Meter:               &meter.Config{},
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, fmt.Errorf("meterd: read %q: %w", path, err)
	}
	if _, err := toml.Decode(string(b), c); err != nil {
		return nil, fmt.Errorf("meterd: parse %q: %w", path, err)
	}
	if c.Meter == nil {
		c.Meter = &meter.Config{}
	}
	c.Meter.Defaults()
	return c, nil
}
