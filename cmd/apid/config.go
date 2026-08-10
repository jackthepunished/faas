// Package main's config — parsed from /etc/faas/apid.toml (or the path
// passed via --config). Each field is independent of every other so a
// partial config file plus defaults produces a working daemon.
//
// This is the issue-#678 extraction surface (PR-0): every TOML field
// listed here is sourced from the same file that billingloader.LoadBillingConfigFromPath
// already reads for the [billing] block — so two readers see one source
// of truth. Behaviour-preserving: every inline env read in
// cmd/apid/main.go that used to read FAAS_APID_* or FAAS_GITHUBD_*
// (etc.) now goes through one of the helpers below; the env vars
// continue to win over the TOML value because the helpers are called
// after LoadConfig in main.go and the env-overlay pattern is preserved.
//
// PR-0 only adds the type, LoadConfig, the Get helpers, and the Load*
// TLS helpers (mirroring cmd/vmmd/config.go's shape). PR-A adds the
// *WithVerifier variants; PR-B wires the verifier construction. None
// of those land in this PR.
package main

import (
	"crypto/tls"
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
	"github.com/onebox-faas/faas/pkg/role"
	"github.com/onebox-faas/faas/pkg/wire"
)

// Config is the on-disk representation of apid's TOML config.
// File reads use BurntSushi/toml (already a transitive dep of
// many tools; pinning it here makes the daemon's config story
// explicit).
type Config struct {
	// ListenAddr is the loopback bind address for the customer-facing
	// REST API + dashboard. Defaults to 127.0.0.1:8081 (legacy single-
	// box default; gatewayd-public reverse-proxies 0.0.0.0:443 in
	// front). Mirrors the legacy FAAS_APID_LISTEN env var.
	ListenAddr string `toml:"listen_addr"`

	// MetricsAddr is the optional bind address for /metrics. Empty
	// disables the listener. Mirrors FAAS_APID_METRICS_ADDR.
	MetricsAddr string `toml:"metrics_addr"`

	// AdvisorySock is the unix-domain socket the stateless-advisory
	// gRPC server binds when set (vmmd dials /run/faas/apid.sock to
	// forward fanotify batches from guest-init). Empty disables.
	// Mirrors FAAS_APID_ADVISORY_SOCK. Defaults to /run/faas/apid.sock.
	AdvisorySock string `toml:"advisory_sock"`

	// GithubdBridgeSock is the unix-domain socket the githubd → apid
	// build-enqueue bridge gRPC server binds when set. Empty disables.
	// Mirrors FAAS_APID_GITHUBD_BRIDGE_SOCK. Defaults to
	// /run/faas/apid-githubd.sock.
	GithubdBridgeSock string `toml:"githubd_bridge_sock"`

	// GithubdSocket is the unix-domain socket apid dials to call
	// githubd's EnqueueBuild RPC (issue #98 / ADR-028 phase). Empty
	// uses the newGithubdClient stub-client path (every method
	// returns api.Problem{Code:"githubd_not_ready"}). Mirrors
	// FAAS_GITHUBD_SOCKET. Defaults to /run/faas/githubd.sock.
	GithubdSocket string `toml:"githubd_socket"`

	// AppsDomain is the platform wildcard host (e.g. "apps.gregale.dev").
	// apid renders the wildcard-aware /login template that lets the
	// dashboard build <slug>.apps.<domain> links per app. Empty disables
	// the wildcard UI (custom-domain-only deployments).
	// Mirrors FAAS_APPS_DOMAIN. No default.
	AppsDomain string `toml:"apps_domain"`

	// Server-mTLS material for the advisory listener (ADR-052 /
	// issue #95). All three paths empty => no TLS, single-box unix
	// socket path; all three set => RequireAndVerifyClientCert for
	// the multi-box tcp/dns path. Partial cluster => startup error
	// naming the missing fields. Mirrors
	// FAAS_APID_ADVISORY_TLS_{CERT,KEY,CA}_PATH env trio.
	AdvisoryTLSCertPath string `toml:"advisory_tls_cert_path"`
	AdvisoryTLSKeyPath  string `toml:"advisory_tls_key_path"`
	AdvisoryTLSCAPath   string `toml:"advisory_tls_ca_path"`

	// GithubdBridgeServerTLS is the server-mTLS material for the
	// githubd → apid bridge listener (ADR-052). Same partial-cluster
	// contract as AdvisoryTLS*. Mirrors
	// FAAS_APID_GITHUBD_BRIDGE_TLS_{CERT,KEY,CA}_PATH env trio.
	GithubdBridgeTLSCertPath string `toml:"githubd_bridge_tls_cert_path"`
	GithubdBridgeTLSKeyPath  string `toml:"githubd_bridge_tls_key_path"`
	GithubdBridgeTLSCAPath   string `toml:"githubd_bridge_tls_ca_path"`

	// GithubdClientTLS is the client-mTLS material apid uses to
	// dial githubd's EnqueueBuild gRPC server (ADR-052). Same
	// partial-cluster contract as AdvisoryTLS*. Mirrors
	// FAAS_GITHUBD_TLS_{CERT,KEY,CA}_PATH env trio.
	GithubdClientTLSCertPath string `toml:"githubd_tls_cert_path"`
	GithubdClientTLSKeyPath  string `toml:"githubd_tls_key_path"`
	GithubdClientTLSCAPath   string `toml:"githubd_tls_ca_path"`

	// NodeName is the multi-box identity for the apid process
	// (issue #678 / ADR-093 PR-0). When non-empty, apid is in
	// multi-box mode: PR-B constructs PGNodeVerifier and threads
	// it through every Load*WithVerifier helper. When empty,
	// the verifier stays nil and stdlib trust alone runs (the
	// single-box dev back-compat path). Operator seeds the
	// matching row in compute_nodes via the existing
	// POST /v1/compute-nodes flow (no new apid handler —
	// reuses UpsertComputeNodeFromOperator). Defaults to "".
	NodeName string `toml:"node_name"`

	// Role is the box shape this apid inhabits (Gate-B; env
	// override FAAS_APID_ROLE wins when set). apid is a
	// control-plane daemon — it refuses to start under
	// RoleComputeOnly. RoleSingleBox is the default and lets
	// single-box dev boot unmoved.
	Role role.Role `toml:"role"`
}

// LoadConfig reads a TOML file at path and returns the parsed Config
// with defaults filled in. A missing file is not an error if defaults
// suffice; in that case an empty config is returned.
//
// Env overlay pattern (preserved from the pre-PR-#678 inline reads):
// main.go calls LoadConfig first, then re-applies FAAS_APID_* /
// FAAS_GITHUBD_* env vars on top via the Get helpers (ListenAddr()
// etc.). The env-var precedence over TOML is load-bearing for the
// containerised-deploys path (no TOML in those images).
func LoadConfig(path string) (*Config, error) {
	c := &Config{
		// Defaults match the legacy FAAS_APID_LISTEN,
		// FAAS_APID_ADVISORY_SOCK, FAAS_APID_GITHUBD_BRIDGE_SOCK
		// and FAAS_GITHUBD_SOCKET values so a partial / missing
		// toml behaves the same as the pre-PR-#678 inline-env path.
		// PR-0 is behaviour-preserving; these defaults keep
		// single-box dev booting unchanged.
		ListenAddr:        "127.0.0.1:8081",
		AdvisorySock:      "/run/faas/apid.sock",
		GithubdBridgeSock: "/run/faas/apid-githubd.sock",
		GithubdSocket:     "/run/faas/githubd.sock",
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Gate-B: even on the missing-file path, resolve Role
			// against FAAS_APID_ROLE so env wins over the empty
			// TOML default. role.FromConfig falls back to
			// RoleSingleBox when the env is unset.
			c.Role = role.FromConfig(string(c.Role), "FAAS_APID_ROLE")
			return c, nil
		}
		return nil, fmt.Errorf("apid: read %q: %w", path, err)
	}
	if err := toml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("apid: parse %q: %w", path, err)
	}
	// Gate-B: resolve Role AFTER toml.Unmarshal so the post-decode
	// c.Role is consulted against FAAS_APID_ROLE. Setting Role in
	// the defaults-struct literal lets toml.Unmarshal overwrite it,
	// which would silently make the env override dead. The role
	// gate at boot calls role.Require to refuse to start under
	// the wrong box shape.
	c.Role = role.FromConfig(string(c.Role), "FAAS_APID_ROLE")
	return c, nil
}

// GetListenAddr returns the listen address with env-var overlay
// (FAAS_APID_LISTEN wins over TOML). Single-box default 127.0.0.1:8081
// is the legacy loopback bind.
func (c *Config) GetListenAddr(env func(string) string) string {
	if v := env("FAAS_APID_LISTEN"); v != "" {
		return v
	}
	return c.ListenAddr
}

// GetMetricsAddr returns the metrics bind address with env-var
// overlay (FAAS_APID_METRICS_ADDR wins over TOML). Empty disables the
// listener (the scrape observer stays wired; only the bind is skipped).
func (c *Config) GetMetricsAddr(env func(string) string) string {
	if v := env("FAAS_APID_METRICS_ADDR"); v != "" {
		return v
	}
	return c.MetricsAddr
}

// GetAdvisorySock returns the advisory socket path with env-var
// overlay (FAAS_APID_ADVISORY_SOCK wins over TOML). Empty disables
// the listener. Mirrors the legacy resolveAdvisorySock helper in
// cmd/apid/main.go that this PR replaces.
func (c *Config) GetAdvisorySock(env func(string) string) string {
	if v := env("FAAS_APID_ADVISORY_SOCK"); v != "" {
		return v
	}
	return c.AdvisorySock
}

// GetGithubdBridgeSock returns the githubd bridge socket path with
// env-var overlay (FAAS_APID_GITHUBD_BRIDGE_SOCK wins over TOML).
// Empty disables. Mirrors the legacy resolveGithubdBridgeSock helper
// in cmd/apid/main.go that this PR replaces.
func (c *Config) GetGithubdBridgeSock(env func(string) string) string {
	if v := env("FAAS_APID_GITHUBD_BRIDGE_SOCK"); v != "" {
		return v
	}
	return c.GithubdBridgeSock
}

// GetGithubdSocket returns the githubd dial target with env-var
// overlay (FAAS_GITHUBD_SOCKET wins over TOML). Empty falls through
// to newGithubdClient's stub-client path (every method returns
// api.Problem{Code:"githubd_not_ready"}).
func (c *Config) GetGithubdSocket(env func(string) string) string {
	if v := env("FAAS_GITHUBD_SOCKET"); v != "" {
		return v
	}
	return c.GithubdSocket
}

// GetAppsDomain returns the apps-domain with env-var overlay
// (FAAS_APPS_DOMAIN wins over TOML). Empty disables wildcard
// routing in the dashboard login template.
func (c *Config) GetAppsDomain(env func(string) string) string {
	if v := env("FAAS_APPS_DOMAIN"); v != "" {
		return v
	}
	return c.AppsDomain
}

// LoadAdvisoryTLS returns the server mTLS config apid uses on the
// advisory listener (ADR-052). Empty cluster returns (nil, nil);
// partial cluster is rejected with the advisory_tls_* field names
// so an operator can map the error straight to a TOML key. Nil
// receiver tolerates the test seam (tests pass runDeps directly
// without setting preLoadedConfig; the nil-tolerance keeps the
// existing TestRunWithDeps_ListenErrorReturns shape working).
func (c *Config) LoadAdvisoryTLS() (*tls.Config, error) {
	if c == nil {
		return nil, nil
	}
	return wire.LoadServerTLSConfigWithPrefix("advisory_", c.AdvisoryTLSCertPath, c.AdvisoryTLSKeyPath, c.AdvisoryTLSCAPath)
}

// LoadGithubdBridgeTLS returns the server mTLS config apid uses on
// the githubd → apid bridge listener (ADR-052). Empty cluster
// returns (nil, nil); partial cluster is rejected with the
// githubd_bridge_tls_* field names.
func (c *Config) LoadGithubdBridgeTLS() (*tls.Config, error) {
	if c == nil {
		return nil, nil
	}
	return wire.LoadServerTLSConfigWithPrefix("githubd_bridge_", c.GithubdBridgeTLSCertPath, c.GithubdBridgeTLSKeyPath, c.GithubdBridgeTLSCAPath)
}

// LoadGithubdTLS returns the client mTLS config apid uses to dial
// githubd's EnqueueBuild gRPC server (ADR-052). Empty cluster returns
// (nil, nil); partial cluster is rejected with the githubd_tls_*
// field names.
func (c *Config) LoadGithubdTLS() (*tls.Config, error) {
	if c == nil {
		return nil, nil
	}
	return wire.LoadClientTLSConfigWithPrefix("githubd_", c.GithubdClientTLSCertPath, c.GithubdClientTLSKeyPath, c.GithubdClientTLSCAPath)
}
