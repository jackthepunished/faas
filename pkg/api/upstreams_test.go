package api

import (
	"testing"
)

// TestDataUpstreamKindIsValid_AllFourteen pins the closed-vocab
// table. A regression that drops a kind (or adds a typo) breaks
// the §11 wire shape and surfaces a 23514 at INSERT.
func TestDataUpstreamKindIsValid_AllFourteen(t *testing.T) {
	all := []DataUpstreamKind{
		DataUpstreamKindPostgres, DataUpstreamKindRedis, DataUpstreamKindMongo,
		DataUpstreamKindCassandra, DataUpstreamKindClickhouse,
		DataUpstreamKindElasticsearch, DataUpstreamKindOpensearch,
		DataUpstreamKindRabbitmq, DataUpstreamKindKafka, DataUpstreamKindNats,
		DataUpstreamKindMinio, DataUpstreamKindMemcached, DataUpstreamKindEtcd,
		DataUpstreamKindS3, DataUpstreamKindHTTPSAPI,
	}
	if len(all) != 15 {
		t.Errorf("closed vocab size: got %d, want 15", len(all))
	}
	for _, k := range all {
		if !DataUpstreamKindIsValid(k) {
			t.Errorf("DataUpstreamKindIsValid(%q) = false, want true", k)
		}
	}
	// Closed-set tripwire — an arbitrary unknown kind must be
	// rejected. Mirrors the SQL CHECK.
	if DataUpstreamKindIsValid("not-a-kind") {
		t.Error("DataUpstreamKindIsValid(\"not-a-kind\") = true, want false")
	}
	if DataUpstreamKindIsValid("") {
		t.Error("DataUpstreamKindIsValid(\"\") = true, want false")
	}
}

// TestDefaultPortForKind_AllKindsHaveDefaults pins that every
// kind in the closed vocab has an IANA-registered default port.
// A regression that adds a new kind without a port default
// forces the env-classifier to fail closed at the API boundary
// rather than silently INSERT port=0 (which would trip the
// migration CHECK).
func TestDefaultPortForKind_AllKindsHaveDefaults(t *testing.T) {
	all := []DataUpstreamKind{
		DataUpstreamKindPostgres, DataUpstreamKindRedis, DataUpstreamKindMongo,
		DataUpstreamKindCassandra, DataUpstreamKindClickhouse,
		DataUpstreamKindElasticsearch, DataUpstreamKindOpensearch,
		DataUpstreamKindRabbitmq, DataUpstreamKindKafka, DataUpstreamKindNats,
		DataUpstreamKindMinio, DataUpstreamKindMemcached, DataUpstreamKindEtcd,
		DataUpstreamKindS3, DataUpstreamKindHTTPSAPI,
	}
	for _, k := range all {
		port, ok := DefaultPortForKind(k)
		if !ok {
			t.Errorf("DefaultPortForKind(%q) = !ok; want a default port", k)
			continue
		}
		if port < 1 || port > 65535 {
			t.Errorf("DefaultPortForKind(%q) = %d, want [1, 65535]", k, port)
		}
	}
}

// TestValidateUpstreamHost_HappyPath walks a few representative
// valid hostnames through ValidateUpstreamHost. Pin the
// RFC-952/1123 acceptance shape.
func TestValidateUpstreamHost_HappyPath(t *testing.T) {
	cases := []string{
		"db.example.com",
		"a.b.c.example.com",
		"my-db.cluster-1.us-east-2.aws.neon.tech",
		"single",
		"x.y",
		"abc123",
		"a-b-c.example.com",
	}
	for _, host := range cases {
		if p := ValidateUpstreamHost(host); p != nil {
			t.Errorf("ValidateUpstreamHost(%q) = %v, want nil", host, p)
		}
	}
}

// TestValidateUpstreamHost_Rejections walks the rejection matrix:
// empty, oversized, wildcard, IPv4 literal, mixed case, leading
// or trailing dash, underscore. All must return a *Problem with
// CodeUpstreamInvalidHost.
func TestValidateUpstreamHost_Rejections(t *testing.T) {
	cases := []struct {
		name string
		host string
	}{
		{"empty", ""},
		{"oversized_254", "a" + repeat("a", 252) + ".com"},
		{"wildcard", "*.example.com"},
		{"ipv4", "192.168.1.1"},
		{"ipv4_simple", "10.0.0.1"},
		{"uppercase", "DB.EXAMPLE.COM"},
		{"underscore", "db_example.com"},
		{"leading_dash", "-db.example.com"},
		{"trailing_dash", "db-.example.com"},
		{"trailing_dot", "db.example.com."},
		{"space", "db .example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := ValidateUpstreamHost(tc.host)
			if p == nil {
				t.Fatalf("ValidateUpstreamHost(%q) = nil, want *Problem", tc.host)
			}
			if p.Code != CodeUpstreamInvalidHost {
				t.Errorf("ValidateUpstreamHost(%q).Code = %q, want %q",
					tc.host, p.Code, CodeUpstreamInvalidHost)
			}
		})
	}
}

// TestValidateUpstreamPort walks the port range check.
func TestValidateUpstreamPort(t *testing.T) {
	// Valid
	for _, p := range []int{1, 80, 443, 5432, 6379, 65535} {
		if prob := ValidateUpstreamPort(p); prob != nil {
			t.Errorf("ValidateUpstreamPort(%d) = %v, want nil", p, prob)
		}
	}
	// Invalid
	for _, p := range []int{0, -1, 65536, 100000} {
		prob := ValidateUpstreamPort(p)
		if prob == nil {
			t.Errorf("ValidateUpstreamPort(%d) = nil, want *Problem", p)
			continue
		}
		if prob.Code != CodeUpstreamInvalidPort {
			t.Errorf("ValidateUpstreamPort(%d).Code = %q, want %q",
				p, prob.Code, CodeUpstreamInvalidPort)
		}
	}
}

// TestPutDataUpstreamRequest_Validate walks the body validator.
func TestPutDataUpstreamRequest_Validate(t *testing.T) {
	good := PutDataUpstreamRequest{
		Kind:  DataUpstreamKindPostgres,
		Host:  "db.example.com",
		Port:  5432,
		Scope: "default",
	}
	if p := good.Validate(); p != nil {
		t.Errorf("good request Validate() = %v, want nil", p)
	}
	// Empty host
	r := good
	r.Host = ""
	if p := r.Validate(); p == nil || p.Code != CodeUpstreamInvalidHost {
		t.Errorf("empty host: got %v, want *Problem with CodeUpstreamInvalidHost", p)
	}
	// Bad port
	r = good
	r.Port = 0
	if p := r.Validate(); p == nil || p.Code != CodeUpstreamInvalidPort {
		t.Errorf("port 0: got %v, want *Problem with CodeUpstreamInvalidPort", p)
	}
	// Bad kind
	r = good
	r.Kind = "not-a-kind"
	if p := r.Validate(); p == nil || p.Code != CodeUpstreamInvalidKind {
		t.Errorf("bad kind: got %v, want *Problem with CodeUpstreamInvalidKind", p)
	}
	// Bad scope
	r = good
	r.Scope = "__all__"
	if p := r.Validate(); p == nil {
		t.Errorf("__all__ scope: got nil, want *Problem (rejected by ValidateScope)")
	}
}

// TestDataUpstreamHostLast4 pins the §11 last-4-fragment
// behaviour. The fragment is the only operator-visible piece of
// the plaintext host on the wire; the full plaintext is never
// returned.
func TestDataUpstreamHostLast4(t *testing.T) {
	cases := []struct {
		host string
		want string
	}{
		{"db.example.com", ".com"},
		{"neon", "neon"},
		{"ab", "ab"},
		{"", ""},
		{"a", "a"},
	}
	for _, tc := range cases {
		if got := DataUpstreamHostLast4(tc.host); got != tc.want {
			t.Errorf("DataUpstreamHostLast4(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
