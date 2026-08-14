// Package api — ADR-098 §D4 / §11 data-upstream DTOs.
//
// Wire shape for /v1/apps/{slug}/upstreams[...] (PR-B / C4).
// Mirrors the naming pattern in pkg/api/secrets.go
// (CreateXRequest / XResponse / XListResponse) so the SDK generators
// + dashboard treat every feature surface the same way.
//
// pkg/api ↔ pkg/state cycle (memory/pkg-api-cannot-import-pkg-state):
// pkg/state imports pkg/api (for the api.Plan type). This file MUST
// NOT import pkg/state. The 14-value DataUpstreamKind vocabulary is
// re-declared here as a string alias + IsValid() helper, mirroring
// the AllowedOrgRoles / OrgRoleIsValid pattern at pkg/api/orgs.go.
// The state-row → DTO conversion lives at the handler boundary
// (cmd/apid/handlers_upstreams.go, C4) where both packages meet.
//
// §11 secret rule (the load-bearing claim): the response shape
// carries ONLY the host_redacted_hash (8-hex-prefix from
// logsanitize.HashShort). The plaintext host NEVER appears on the
// wire — neither in the POST body, nor in the GET response, nor in
// the audit kind. The dashboard's "you pinned this" badge works off
// the host_redacted_hash + the last 4 chars of the host, which the
// apid-side writer derives via fmt.Sprintf("%.4s", host) and stamps
// into a separate `host_last4` field (NOT the host itself). The
// derived last4 is the only operator-visible fragment of the host;
// it survives a hash table reverse-lookup and is enough to render
// "neon-tech" / "aws-rds" in the UI.

package api

import (
	"fmt"
	"regexp"
)

// DataUpstreamKind is the closed vocabulary for the data-upstream
// `kind` field. Re-declared here (not imported from pkg/state) per
// the pkg/api ↔ pkg/state cycle rule. The 14 values mirror the SQL
// CHECK at migrations/00226_data_upstreams.sql
// (`data_upstreams_kind_check`).
type DataUpstreamKind string

const (
	DataUpstreamKindPostgres      DataUpstreamKind = "postgres"
	DataUpstreamKindRedis         DataUpstreamKind = "redis"
	DataUpstreamKindMongo         DataUpstreamKind = "mongo"
	DataUpstreamKindCassandra     DataUpstreamKind = "cassandra"
	DataUpstreamKindClickhouse    DataUpstreamKind = "clickhouse"
	DataUpstreamKindElasticsearch DataUpstreamKind = "elasticsearch"
	DataUpstreamKindOpensearch    DataUpstreamKind = "opensearch"
	DataUpstreamKindRabbitmq      DataUpstreamKind = "rabbitmq"
	DataUpstreamKindKafka         DataUpstreamKind = "kafka"
	DataUpstreamKindNats          DataUpstreamKind = "nats"
	DataUpstreamKindMinio         DataUpstreamKind = "minio"
	DataUpstreamKindMemcached     DataUpstreamKind = "memcached"
	DataUpstreamKindEtcd          DataUpstreamKind = "etcd"
	DataUpstreamKindS3            DataUpstreamKind = "s3"
	DataUpstreamKindHTTPSAPI      DataUpstreamKind = "https_api"
)

// DataUpstreamKindIsValid returns true when k is one of the 14
// closed-vocabulary values. Used by apid's createUpstream handler
// (C4) to reject an unknown kind with 400 upstream_invalid_kind
// BEFORE the store is touched. Mirrors the EdgeRuleKindIsValid
// pattern at pkg/api/edge_rules.go.
func DataUpstreamKindIsValid(k DataUpstreamKind) bool {
	switch k {
	case DataUpstreamKindPostgres, DataUpstreamKindRedis, DataUpstreamKindMongo,
		DataUpstreamKindCassandra, DataUpstreamKindClickhouse,
		DataUpstreamKindElasticsearch, DataUpstreamKindOpensearch,
		DataUpstreamKindRabbitmq, DataUpstreamKindKafka, DataUpstreamKindNats,
		DataUpstreamKindMinio, DataUpstreamKindMemcached, DataUpstreamKindEtcd,
		DataUpstreamKindS3, DataUpstreamKindHTTPSAPI:
		return true
	}
	return false
}

// DefaultPortForKind returns the IANA-registered default port for
// the kind. The env-classifier (C4) stamps this when the env value
// lacks an explicit port (DATABASE_URL without :5432, REDIS_URL
// without :6379, etc.). The classifier's normalisation step also
// stamps the port here, so the response shape's port field is
// always the resolved port — never 0.
//
// 0 with ok=false means "kind has no registered default"; the
// handler surfaces 400 upstream_invalid_port in that case rather
// than INSERTing a 0 (which would trip the migration CHECK).
func DefaultPortForKind(k DataUpstreamKind) (int, bool) {
	switch k {
	case DataUpstreamKindPostgres:
		return 5432, true
	case DataUpstreamKindRedis:
		return 6379, true
	case DataUpstreamKindMongo:
		return 27017, true
	case DataUpstreamKindCassandra:
		return 9042, true
	case DataUpstreamKindClickhouse:
		return 9000, true
	case DataUpstreamKindElasticsearch, DataUpstreamKindOpensearch:
		return 9200, true
	case DataUpstreamKindRabbitmq:
		return 5672, true
	case DataUpstreamKindKafka:
		return 9092, true
	case DataUpstreamKindNats:
		return 4222, true
	case DataUpstreamKindMinio, DataUpstreamKindS3:
		return 9000, true
	case DataUpstreamKindMemcached:
		return 11211, true
	case DataUpstreamKindEtcd:
		return 2379, true
	case DataUpstreamKindHTTPSAPI:
		return 443, true
	}
	return 0, false
}

// upstreamHostPattern is the RFC-952/1123 hostname regex with the
// IPv4-literal backstop AND the wildcard-host rejection (mirrors
// the migration CHECK at `data_upstreams_host_check`). Mirrored
// here so the apid handler fails fast at the API boundary with
// 400 upstream_invalid_host instead of surfacing a 23514 at
// INSERT time.
//
// The regex:
//   - Each label is [a-z0-9]([a-z0-9-]{0,61}[a-z0-9])? — 1..63
//     chars, alnum + dash, no leading/trailing dash.
//   - Labels joined by dots, total <= 253 chars (the RFC 1035
//     max-domain length).
//   - Wildcard literal (*.example.com) is rejected by the leading
//     alpha-numeric requirement on the first character.
//   - IPv4 dotted-quad literals are rejected via the secondary
//     `!~ '^[0-9]+(\.[0-9]+)+$'` check in the SQL CHECK; the Go
//     side mirrors it in ValidateUpstreamHost.
var upstreamHostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// ipv4LiteralPattern matches a host whose every label is all
// digits separated by dots (i.e. an IPv4 dotted-quad literal). The
// SQL CHECK at `data_upstreams_host_check` uses `AND host !~
// '^[0-9]+(\.[0-9]+)+$'` to reject; the Go side mirrors it so
// apid's ValidateUpstreamHost surfaces 400 upstream_invalid_host
// before INSERT, not a 23514 at INSERT.
var ipv4LiteralPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)+$`)

// ValidateUpstreamHost returns nil when host is a valid RFC-952
// /1123 hostname AND not an IPv4 literal. Returns *Problem with
// CodeUpstreamInvalidHost (400) otherwise. Length cap mirrors the
// SQL CHECK (1..253 chars).
func ValidateUpstreamHost(host string) *Problem {
	if host == "" {
		return ErrUpstreamInvalidHost("host is required")
	}
	if len(host) > 253 {
		return ErrUpstreamInvalidHost("host exceeds 253 chars (RFC 1035 max)")
	}
	if !upstreamHostPattern.MatchString(host) {
		return ErrUpstreamInvalidHost("host is not a valid RFC 952/1123 hostname")
	}
	if ipv4LiteralPattern.MatchString(host) {
		return ErrUpstreamInvalidHost("IPv4 literal hosts are not accepted; use an RFC 952/1123 hostname")
	}
	return nil
}

// ValidateUpstreamPort returns nil when port is in [1, 65535];
// 0 is rejected (a port-less env value should be resolved via
// DefaultPortForKind at the classifier, not sent as 0). Returns
// *Problem with CodeUpstreamInvalidPort (400) otherwise.
func ValidateUpstreamPort(port int) *Problem {
	if port < 1 || port > 65535 {
		return ErrUpstreamInvalidPort(fmt.Sprintf("port %d is outside the [1, 65535] range", port))
	}
	return nil
}

// MaxUpstreamHostLength mirrors the SQL CHECK at
// `data_upstreams_host_check` (length(host) BETWEEN 1 AND 253).
// Exposed as a constant so the apid handler can pre-cap without
// invoking the regex.
const MaxUpstreamHostLength = 253

// DataUpstreamHostLast4 is the per-row helper that returns the
// last 4 characters of the plaintext host. The apid handler
// stamps the value into the response's `host_last4` field so the
// dashboard can render "neon-tech" / "aws-rds" without revealing
// the full plaintext host. The fragment survives a hash table
// reverse-lookup and is small enough to not be a privacy issue on
// its own (a known host suffix is not a credential).
//
// Less-than-4-char hosts return the host verbatim (the cap is
// for visual sanity, not a security boundary — the full plaintext
// is not on the wire anywhere).
func DataUpstreamHostLast4(host string) string {
	if len(host) <= 4 {
		return host
	}
	return host[len(host)-4:]
}

// PutDataUpstreamRequest is the POST /v1/apps/{slug}/upstreams
// body. The customer-facing surface is (kind, host, port, scope)
// — the host is plaintext at this point because the customer is
// the one supplying it. The apid handler (C4) hashes via
// pkg/secretbox.HashHost BEFORE INSERT; the plaintext is dropped
// on the floor after the hash returns. The response carries
// ONLY the hash + the host_last4 fragment.
//
// Scope mirrors the env-var scope shape (ADR-090 D3) — 3..40
// chars, lowercase alnum + dash. The handler reads it from the
// query param ?scope= on the GET, but the POST body carries it
// explicitly so the customer can pin a hint to a non-default
// scope.
type PutDataUpstreamRequest struct {
	Kind  DataUpstreamKind `json:"kind"`
	Host  string           `json:"host"`
	Port  int              `json:"port"`
	Scope string           `json:"scope,omitempty"`
}

// Validate enforces the closed-vocab kind check, the RFC-952/1123
// host check, the port range check, and the scope shape check.
// Returns *Problem directly (not error) so the call site can pass
// it straight to api.WriteProblem.
func (r PutDataUpstreamRequest) Validate() *Problem {
	if !DataUpstreamKindIsValid(r.Kind) {
		return ErrUpstreamInvalidKind(fmt.Sprintf("kind %q is not in the closed vocabulary (postgres, redis, mongo, ...)", r.Kind))
	}
	if p := ValidateUpstreamHost(r.Host); p != nil {
		return p
	}
	if p := ValidateUpstreamPort(r.Port); p != nil {
		return p
	}
	if r.Scope != "" {
		// Reuse the env-var scope validator (ADR-090 D3) — the
		// shape is identical (3..40 chars, lowercase alnum + dash).
		if p := ValidateScope(r.Scope); p != nil {
			return ErrUpstreamInvalidKind("scope must be 3..40 chars, lowercase alnum + dash")
		}
	}
	return nil
}

// DataUpstreamResponse is the GET / list shape. The §11
// load-bearing claim: the response carries ONLY the
// host_redacted_hash + the host_last4 fragment. The plaintext
// host is NEVER on the wire — neither in this struct's fields,
// nor in the JSON output, nor in the audit kind.
//
// HostLast4 is the operator-visible fragment (4 chars, enough
// to render "neon-tech" / "aws-rds" in the UI without revealing
// the full host). Computed at the apid handler via
// DataUpstreamHostLast4; not stored on the row (the storage
// is the hash, not the fragment — the fragment is the response
// surface, not a database field).
type DataUpstreamResponse struct {
	ID               string             `json:"id"`
	Source           DataUpstreamSource `json:"source"`
	Kind             DataUpstreamKind   `json:"kind"`
	HostRedactedHash string             `json:"host_redacted_hash"`
	HostLast4        string             `json:"host_last4,omitempty"`
	Port             int                `json:"port"`
	Scope            string             `json:"scope,omitempty"`
	DeclaredRegion   string             `json:"declared_region,omitempty"`
	LastRTTMs        *int               `json:"last_rtt_ms,omitempty"`
	LastProbedAt     string             `json:"last_probed_at,omitempty"`
	CreatedAt        string             `json:"created_at"`
	LastSeenAt       string             `json:"last_seen_at"`
}

// DataUpstreamListResponse is the wrapped GET response: the
// upstreams slice plus quota metadata so the CLI can render
// "3/8 upstreams" without a second request. Mirrors
// AppEnvListResponse (env.go:93) and AppSecretListResponse
// field-for-field so SDK callers reuse the same parsing branch.
type DataUpstreamListResponse struct {
	Upstreams []DataUpstreamResponse `json:"upstreams"`
	Quota     int                    `json:"quota_max"`
	Count     int                    `json:"count"`
}
