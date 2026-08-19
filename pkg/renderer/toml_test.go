package renderer

import (
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/manifest"
)

// fixtureTOML returns a manifest.DaemonConfig populated for the
// given daemon. The schema is small enough that a single helper
// covers the renderer tests.
func fixtureTOML(daemon string) *manifest.DaemonConfig {
	switch daemon {
	case "schedd":
		return &manifest.DaemonConfig{
			Bind: "unix:///run/faas/schedd.sock",
			Outbound: &manifest.OutboundConfig{
				Target: "unix:///run/faas/vmmd.sock",
			},
		}
	case "apid":
		return &manifest.DaemonConfig{
			Bind: "unix:///run/faas/apid.sock",
		}
	case "gatewayd-internal":
		return &manifest.DaemonConfig{
			Bind: "tcp://0.0.0.0:9090",
		}
	case "vmmd":
		return &manifest.DaemonConfig{
			Bind: "unix:///run/faas/vmmd.sock",
		}
	}
	return &manifest.DaemonConfig{}
}

func TestRenderTOML_Schedd(t *testing.T) {
	body, flat, err := renderTOML(tomlRenderCtx{
		Daemon: "schedd",
		DC:     fixtureTOML("schedd"),
	})
	if err != nil {
		t.Fatalf("renderTOML: %v", err)
	}
	if len(body) == 0 {
		t.Errorf("body is empty")
	}
	// Validator-aligned keys per HostKeys["schedd"].
	for _, want := range []string{
		"socket_path = \"/run/faas/schedd.sock\"",
		"db_url = \"\"", // empty value omitted by writeTOMLKV
	} {
		if strings.Contains(string(body), "db_url") && !strings.Contains(string(body), want) {
			t.Errorf("body missing %q\nbody:\n%s", want, body)
		}
	}
	// Renderer must NOT emit a [compute_node] block for schedd.
	if strings.Contains(string(body), "[compute_node]") {
		t.Errorf("schedd body unexpectedly contains [compute_node]\nbody:\n%s", body)
	}
	// schedd's PrivateKeys do not include apps_domain (per the
	// HostKeys catalog); an AppsDomain value flows through but
	// writeTOMLKV skips it because schedd doesn't declare it.
	if strings.Contains(string(body), "apps_domain") {
		t.Errorf("schedd body unexpectedly contains apps_domain\nbody:\n%s", body)
	}
	// FlatMap keys must match HostKeys.
	host := manifest.HostKeys["schedd"]
	for _, k := range host.PrivateKeys {
		if _, ok := flat[k]; !ok {
			t.Errorf("flatMap missing top-level key %q", k)
		}
	}
}

func TestRenderTOML_Apid(t *testing.T) {
	body, _, err := renderTOML(tomlRenderCtx{
		Daemon: "apid",
		DC:     fixtureTOML("apid"),
	})
	if err != nil {
		t.Fatalf("renderTOML: %v", err)
	}
	if !strings.Contains(string(body), "socket_path = \"/run/faas/apid.sock\"") {
		t.Errorf("apid body missing socket_path\nbody:\n%s", body)
	}
}

func TestRenderTOML_GatewaydInternal(t *testing.T) {
	// gatewayd-internal uses tcp:// bind → listen_addr, not socket_path.
	dc := &manifest.DaemonConfig{
		Bind: "tcp://0.0.0.0:9090",
	}
	body, flat, err := renderTOML(tomlRenderCtx{
		Daemon: "gatewayd-internal",
		DC:     dc,
	})
	if err != nil {
		t.Fatalf("renderTOML: %v", err)
	}
	if !strings.Contains(string(body), "listen_addr = \"0.0.0.0:9090\"") {
		t.Errorf("body missing listen_addr\nbody:\n%s", body)
	}
	if _, ok := flat["listen_addr"]; !ok {
		t.Errorf("flatMap missing listen_addr")
	}
	if v := flat["socket_path"]; v != "" {
		t.Errorf("flatMap socket_path = %q, want empty (tcp bind)", v)
	}
	// gatewayd-internal listens on 9090; its metrics_addr is the
	// canonical Prometheus port for that daemon — NOT the
	// catch-all 9091.
	if !strings.Contains(string(body), "metrics_addr = \"127.0.0.1:9090\"") {
		t.Errorf("gatewayd-internal body missing metrics_addr 9090\nbody:\n%s", body)
	}
}

func TestRenderTOML_VmmdWithComputeNode(t *testing.T) {
	// vmmd has both PrivateKeys AND ComputeNodeBlock. Confirm
	// both flow into the flatMap and the body has the [compute_node]
	// section.
	body, flat, err := renderTOML(tomlRenderCtx{
		Daemon:   "vmmd",
		DC:       fixtureTOML("vmmd"),
		HostSANs: []string{"vmmd-1.faas.example.com"},
	})
	if err != nil {
		t.Fatalf("renderTOML: %v", err)
	}
	if !strings.Contains(string(body), "[compute_node]") {
		t.Errorf("vmmd body missing [compute_node]\nbody:\n%s", body)
	}
	// vmmd metrics port is 9095 — the per-daemon table, not 9091.
	if !strings.Contains(string(body), "metrics_addr = \"127.0.0.1:9095\"") {
		t.Errorf("vmmd body missing metrics_addr 9095\nbody:\n%s", body)
	}
	// Validator walk: flatMap must contain every HostKeys.ComputeNodeBlock
	// key under the "compute_node." prefix.
	host := manifest.HostKeys["vmmd"]
	for _, k := range host.ComputeNodeBlock {
		prefixed := k.Table + "." + k.Key
		if _, ok := flat[prefixed]; !ok {
			t.Errorf("flatMap missing table key %q", prefixed)
		}
	}
}

func TestRenderTOML_VmmdDerivesIdentityAndTargetFromHost(t *testing.T) {
	body, flat, err := renderTOML(tomlRenderCtx{
		Daemon:      "vmmd",
		DC:          fixtureTOML("vmmd"),
		HostName:    "fsn-2",
		HostAddress: "10.42.0.2:50051",
	})
	if err != nil {
		t.Fatalf("renderTOML: %v", err)
	}
	if got := flat["compute_node.name"]; got != "fsn-2" {
		t.Errorf("compute_node.name = %q, want fsn-2", got)
	}
	if got := flat["compute_node.target_url"]; got != "tcp://vmmd.faas:50051" {
		t.Errorf("compute_node.target_url = %q, want tcp://vmmd.faas:50051", got)
	}
	if got := flat["compute_node.overlay_ip"]; got != "10.42.0.2" {
		t.Errorf("compute_node.overlay_ip = %q, want 10.42.0.2", got)
	}
	if strings.Contains(string(body), "tcp://0.0.0.0:50051") {
		t.Error("rendered vmmd target_url contains wildcard bind address")
	}
}

func TestRenderTOML_NilDaemonConfig(t *testing.T) {
	_, _, err := renderTOML(tomlRenderCtx{Daemon: "schedd", DC: nil})
	if err == nil {
		t.Errorf("nil DaemonConfig = nil err, want error")
	}
}

func TestRenderTOML_UnknownDaemon(t *testing.T) {
	_, _, err := renderTOML(tomlRenderCtx{Daemon: "no-such-daemon", DC: &manifest.DaemonConfig{}})
	if err == nil {
		t.Errorf("unknown daemon = nil err, want error")
	}
	if err != nil && !strings.Contains(err.Error(), "HostKeys") {
		t.Errorf("err = %v, want HostKeys drift message", err)
	}
}

// TestRenderTOML_PerDaemonMetricsAddr pins the per-daemon metrics
// port table. A future refactor that moves the port to
// daemonunitspec.Entry must keep these mappings.
func TestRenderTOML_PerDaemonMetricsAddr(t *testing.T) {
	cases := []struct {
		daemon string
		want   string
	}{
		{"vmmd", "127.0.0.1:9095"},
		{"gatewayd-internal", "127.0.0.1:9090"},
		{"gatewayd-public", "127.0.0.1:8080"},
		{"apid", "127.0.0.1:9091"},
		{"schedd", "127.0.0.1:9091"},
		{"meterd", "127.0.0.1:9091"},
		{"githubd", "127.0.0.1:9091"},
		{"imaged", "127.0.0.1:9091"},
	}
	for _, c := range cases {
		got := defaultMetricsAddrForDaemon(c.daemon)
		if got != c.want {
			t.Errorf("defaultMetricsAddrForDaemon(%q) = %q, want %q", c.daemon, got, c.want)
		}
	}
}

// TestRenderTOML_AppsDomainFlowsThrough pins that the manifest's
// DNS.AppsDomain flows into apid + gatewayd-internal TOMLs (the two
// daemons whose HostKeys declare apps_domain). An empty AppsDomain
// omits the key, matching the daemon's env-var fallback.
func TestRenderTOML_AppsDomainFlowsThrough(t *testing.T) {
	for _, d := range []string{"apid", "gatewayd-internal"} {
		body, _, err := renderTOML(tomlRenderCtx{
			Daemon:     d,
			DC:         fixtureTOML(d),
			AppsDomain: "apps.gregale.dev",
		})
		if err != nil {
			t.Fatalf("%s: renderTOML: %v", d, err)
		}
		if !strings.Contains(string(body), `apps_domain = "apps.gregale.dev"`) {
			t.Errorf("%s body missing apps_domain\nbody:\n%s", d, body)
		}
	}
	// Empty apps_domain → writeTOMLKV omits the key entirely.
	body, _, err := renderTOML(tomlRenderCtx{
		Daemon:     "apid",
		DC:         fixtureTOML("apid"),
		AppsDomain: "",
	})
	if err != nil {
		t.Fatalf("renderTOML: %v", err)
	}
	if strings.Contains(string(body), "apps_domain") {
		t.Errorf("apid body should not contain apps_domain when empty\nbody:\n%s", body)
	}
}

func TestRenderTOML_DBURLFlowsThrough(t *testing.T) {
	const dsn = "postgres://faas@10.156.0.3:5432/faas?sslmode=disable"
	for _, d := range []string{"apid", "schedd", "vmmd", "imaged"} {
		body, flat, err := renderTOML(tomlRenderCtx{
			Daemon: d,
			DC:     fixtureTOML(d),
			DBURL:  dsn,
		})
		if err != nil {
			t.Fatalf("%s: renderTOML: %v", d, err)
		}
		if got := flat["db_url"]; got != dsn {
			t.Errorf("%s flat db_url = %q, want %q", d, got, dsn)
		}
		if !strings.Contains(string(body), `db_url = "`+dsn+`"`) {
			t.Errorf("%s body missing db_url\nbody:\n%s", d, body)
		}
	}
}

func TestValidateTOMLPlacement_CatchesTombstone(t *testing.T) {
	// Synthetic flatMap: top-level + a tombstone hit on the
	// "compute_node.tls_cert_path" tombstone. The validator must
	// reject this — the renderer's #1 hard fail.
	flat := map[string]string{
		// legit keys pass
		"socket_path":   "/run/faas/vmmd.sock",
		"metrics_addr":  "127.0.0.1:9095",
		"tls_cert_path": "/etc/faas/tls/vmmd/server.crt",
		"tls_key_path":  "/etc/faas/tls/vmmd/server.key",
		"tls_ca_path":   "/etc/faas/tls/ca/ca.crt",
		// tombstone: tls_cert_path inside [compute_node]
		"compute_node.tls_cert_path": "/etc/faas/tls/vmmd/server.crt",
	}
	errs := manifest.ValidateTOMLPlacement("vmmd", flat)
	if errs == nil {
		t.Errorf("tombstone hit = nil errs, want error")
	}
}

// TestValidateTOMLPlacement_CatchesPrivateKeyInTable exercises the
// private-key-in-table path. A PrivateKey (top-level) landing
// inside an `[xxx]` table must be rejected.
func TestValidateTOMLPlacement_CatchesPrivateKeyInTable(t *testing.T) {
	flat := map[string]string{
		"socket_path":   "/run/faas/vmmd.sock",
		"metrics_addr":  "127.0.0.1:9095",
		"tls_cert_path": "/etc/faas/tls/vmmd/server.crt",
		"tls_key_path":  "/etc/faas/tls/vmmd/server.key",
		"tls_ca_path":   "/etc/faas/tls/ca/ca.crt",
		// top-level key `socket_path` is in PrivateKeys; placing it
		// inside the [compute_node] table is the table placement bug.
		"compute_node.socket_path": "/run/faas/vmmd.sock",
	}
	errs := manifest.ValidateTOMLPlacement("vmmd", flat)
	if errs == nil {
		t.Errorf("private-key-in-table = nil errs, want error")
	}
}

// TestValidateTOMLPlacement_PassesValidPlacement confirms the
// positive path: a correctly-shaped vmmd TOML passes. Mirrors the
// production-shape rendering.
func TestValidateTOMLPlacement_PassesValidPlacement(t *testing.T) {
	flat := map[string]string{
		"socket_path":                       "/run/faas/vmmd.sock",
		"metrics_addr":                      "127.0.0.1:9095",
		"tls_cert_path":                     "/etc/faas/tls/vmmd/server.crt",
		"tls_key_path":                      "/etc/faas/tls/vmmd/server.key",
		"tls_ca_path":                       "/etc/faas/tls/ca/ca.crt",
		"compute_node.name":                 "vmmd-1",
		"compute_node.target_url":           "unix:///run/faas/vmmd.sock",
		"compute_node.overlay_ip":           "10.42.0.5",
		"compute_node.vpcpus":               "4",
		"compute_node.mem_mb":               "4096",
		"compute_node.max_concurrency":      "20",
		"compute_node.admission_ceiling_mb": "47600",
	}
	if errs := manifest.ValidateTOMLPlacement("vmmd", flat); errs != nil {
		t.Errorf("valid placement = %v, want nil", errs)
	}
}
