// coverage_test.go — fill additional pkg/manifest coverage gaps
// that manifest_test.go deliberately doesn't touch. Targets the
// parse + validate helpers in manifest.go whose error branches
// are missing tests.
//
//   - ParseHostPort error arms: unix prefix, empty, bad port text,
//     out-of-range port, unspecified host.
//   - TCPURL / ServiceName / ServiceTCPURL error propagation.
//   - Errors.Error() empty-slice short-circuit.
//   - Parse invalid TOML path.
//   - validateFleetEndpoints short-circuit on a single-host fleet.
//
// Conventions: whitebox `package manifest` (matches the
// pre-existing manifest_test.go).

package manifest

import (
	"errors"
	"strings"
	"testing"
)

// --- Errors.Error() empty-slice short-circuit -----------------------

func TestErrors_EmptyErrorString(t *testing.T) {
	// manifest.go:556-558 — the validator returns Errors(nil)
	// as a successful no-error indicator; the Error() method
	// short-circuits to "" so callers can `errors.Is(err,
	// ErrInvalid)` without paying the join cost. Pin that.
	var e Errors
	if got := e.Error(); got != "" {
		t.Errorf("empty Errors.Error() = %q, want \"\"", got)
	}
	// And length > 0 surfaces a non-empty string.
	withOne := Errors{{Path: "p", Message: "m"}}
	if got := withOne.Error(); got == "" {
		t.Errorf("non-empty Errors.Error() = %q, want non-empty", got)
	}
}

// --- ParseHostPort error branches ----------------------------------

func TestParseHostPort_ErrorBranches(t *testing.T) {
	cases := []struct {
		raw     string
		contains string // expected substring in err.Error()
	}{
		{"unix:///run/foo.sock", "unix endpoint is only valid for a single-box host"},
		{"", "endpoint is empty"},
		{"hostonly", "must be host:port"}, // no port → net.SplitHostPort error
		{":1234", "empty host"},
		{"host:0", "invalid port"}, // port < 1
		{"host:99999", "invalid port"}, // port > 65535
		{"host:notanumber", "invalid port"}, // non-numeric port
		{"0.0.0.0:8080", "unspecified host"}, // unspecified IP
		{"!!nope@not-a-host:1234", "invalid host"}, // bad hostname
	}
	for _, c := range cases {
		_, _, err := ParseHostPort(c.raw)
		if err == nil {
			t.Errorf("ParseHostPort(%q): err = nil, want %q", c.raw, c.contains)
			continue
		}
		if !strings.Contains(err.Error(), c.contains) {
			t.Errorf("ParseHostPort(%q) = %v, want chain containing %q", c.raw, err, c.contains)
		}
	}
}

func TestParseHostPort_IPLiteral_Happy(t *testing.T) {
	host, port, err := ParseHostPort("10.0.0.1:8080")
	if err != nil {
		t.Fatalf("ParseHostPort IP: %v", err)
	}
	if host != "10.0.0.1" {
		t.Errorf("host = %q", host)
	}
	if port != 8080 {
		t.Errorf("port = %d", port)
	}
}

func TestParseHostPort_Hostname_Happy(t *testing.T) {
	host, port, err := ParseHostPort("node-1.internal:8080")
	if err != nil {
		t.Fatalf("ParseHostPort hostname: %v", err)
	}
	if host != "node-1.internal" {
		t.Errorf("host = %q", host)
	}
	if port != 8080 {
		t.Errorf("port = %d", port)
	}
}

// --- TCPURL / ServiceName / ServiceTCPURL error propagation ------

func TestTCPURL_PropagatesParseError(t *testing.T) {
	if _, err := TCPURL(""); err == nil {
		t.Error("TCPURL(\"\"): err = nil, want propagated ParseHostPort error")
	}
}

func TestTCPURL_Happy(t *testing.T) {
	got, err := TCPURL("10.0.0.1:8080")
	if err != nil {
		t.Fatalf("TCPURL: %v", err)
	}
	if got != "tcp://10.0.0.1:8080" {
		t.Errorf("TCPURL = %q", got)
	}
}

func TestServiceName_UnknownRole(t *testing.T) {
	_, err := ServiceName("not-a-role")
	if err == nil {
		t.Fatal("err = nil, want unknown-role error")
	}
	if !strings.Contains(err.Error(), "not-a-role") {
		t.Errorf("err = %v, want role name in chain", err)
	}
}

func TestServiceName_KnownRoles(t *testing.T) {
	cases := []struct{ role, want string }{
		{"control-plane", "schedd.faas"},
		{"compute-only", "vmmd.faas"},
	}
	for _, c := range cases {
		got, err := ServiceName(c.role)
		if err != nil {
			t.Errorf("ServiceName(%q): %v", c.role, err)
			continue
		}
		if got != c.want {
			t.Errorf("ServiceName(%q) = %q, want %q", c.role, got, c.want)
		}
	}
}

func TestServiceTCPURL_PropagatesParseError(t *testing.T) {
	if _, err := ServiceTCPURL("control-plane", ""); err == nil {
		t.Error("err = nil, want propagated ParseHostPort error")
	}
}

func TestServiceTCPURL_PropagatesServiceNameError(t *testing.T) {
	if _, err := ServiceTCPURL("not-a-role", "10.0.0.1:8080"); err == nil {
		t.Error("err = nil, want propagated ServiceName error")
	}
}

func TestServiceTCPURL_Happy(t *testing.T) {
	got, err := ServiceTCPURL("control-plane", "10.0.0.1:8080")
	if err != nil {
		t.Fatalf("ServiceTCPURL: %v", err)
	}
	if got != "tcp://schedd.faas:8080" {
		t.Errorf("ServiceTCPURL = %q", got)
	}
}

// --- Parse invalid TOML + short-circuits -------------------------

func TestParse_InvalidYAML(t *testing.T) {
	// Pin the parser error path. Manifest.Parse consumes a
	// YAML/TOML/JSON shape; an empty doc must error rather than
	// silently return zero.
	_, err := Parse([]byte("not: [valid: yaml"))
	if err == nil {
		t.Error("err = nil, want parse error")
	}
}

func TestValidate_SchemaVersionMismatch(t *testing.T) {
	// manifest.go:631 — supported-version check. Drive a fresh
	// Manifest whose SchemaVersion is unsupported and pin the
	// validation error format. Note: the path is "schema_version"
	// and the message is "unsupported version..." — the validator
	// places the field name in Path, not Message.
	m := &Manifest{SchemaVersion: "999"}
	errs := m.Validate()
	if errs == nil {
		t.Fatal("errs = nil, want schema-version error")
	}
	found := false
	for _, e := range errs {
		if e.Path == "schema_version" && strings.Contains(e.Message, "unsupported version") {
			found = true
		}
	}
	if !found {
		t.Errorf("schema_version error not surfaced: %v", errs)
	}
}

func TestValidateFleetEndpoints_SingleHostShortCircuit(t *testing.T) {
	// manifest.go:732-734 — a single-host fleet must skip the
	// per-host endpoint check.
	m := &Manifest{
		Fleet: Fleet{
			Hosts: []Host{
				{Address: ""}, // empty would error if reached
			},
		},
	}
	if errs := m.validateFleetEndpoints(); errs != nil {
		t.Errorf("single-host fleet: errs = %v, want nil short-circuit", errs)
	}
}

// --- Errors.Is helper ----------------------------------------------

func TestErrorsIs_ErrInvalid(t *testing.T) {
	// manifest.go:568-570 — Errors.Is targets ErrInvalid so
	// validators can match via errors.Is. The implementation is
	// `return target == ErrInvalid` regardless of `e`; both
	// empty and non-empty Errors satisfy errors.Is(err,
	// ErrInvalid). Pin that contract.
	var errs Errors
	if !errors.Is(errs, ErrInvalid) {
		t.Error("empty Errors Is ErrInvalid = false, want true")
	}
	errs = append(errs, Error{Path: "p", Message: "m"})
	if !errors.Is(errs, ErrInvalid) {
		t.Error("non-empty Errors Is ErrInvalid = false, want true")
	}
	// And a non-ErrInvalid target returns false.
	if errors.Is(errs, errors.New("other")) {
		t.Error("Errors Is ErrInvalid-shaped-but-different = true, want false")
	}
}
