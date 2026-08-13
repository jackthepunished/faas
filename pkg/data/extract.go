// Package data — ADR-098 §D1 / §11 env-classifier primitives.
//
// This file holds the URL parsing + host/port extraction helpers
// used by the env-classifier (infer.go). Distinct from cmd/apid/
// extract.go (the tarball extraction seam — DIFFERENT concept, see
// the PR-cluster outline note in the plan file).
//
// The shape of the helper is deliberately narrow: extract host
// + port from a connection-string-shaped env value
// (DATABASE_URL, REDIS_URL, ...). The classifier at infer.go
// applies these primitives after the env-key-to-kind mapping
// (DATABASE_URL → postgres) resolves.

package data

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// ErrNotAConnectionString is returned by ExtractHostPort when the
// value doesn't parse as a URL with a host. The classifier at
// infer.go surfaces this internally (NOT a user-facing error — the
// classifier's caller drops unknown env values silently and logs a
// debug-level "skipped N env values" message).
var ErrNotAConnectionString = errors.New("data: value is not a connection string with a host")

// KindClosedVocab entries. Used as canonical string values in
// env-key-to-kind mapping below; the wire-level DataUpstreamKind
// pkg/state enum (PR-A) accepts these as input.
const (
	kindPostgres = "postgres"
	kindRedis    = "redis"
)

// schemeKindMap is the env-key-prefix → DataUpstreamKind
// mapping. Order matters in the sense that "postgres" /
// "postgresql" both map to DataUpstreamKindPostgres (the second
// value wins on lookup), but the lookup is by prefix so
// "postgres://" and "postgresql://" both find the kind.
//
// Keyed on scheme (the URL part before ://). Values are the
// closed-vocab kind from pkg/api/upstreams.go. Mirrored in
// pkg/api because pkg/api cannot import pkg/state (memory
// pkg-api-cannot-import-pkg-state) and the env-classifier needs
// the same vocabulary.
var schemeKindMap = map[string]string{
	"postgres":     "postgres",
	"postgresql":   "postgres",
	"redis":        "redis",
	"rediss":       "redis",
	"mongodb":      "mongo",
	"mongodb+srv":  "mongo",
	"cassandra":    "cassandra",
	"clickhouse":   "clickhouse",
	"elasticsearch": "elasticsearch",
	"opensearch":   "opensearch",
	"amqp":         "rabbitmq",
	"amqps":        "rabbitmq",
	"kafka":        "kafka",
	"nats":         "nats",
	"minio":        "minio",
	"s3":           "s3",
	"memcached":    "memcached",
	"etcd":         "etcd",
	"https":        "https_api",
	"http":         "https_api",
}

// KindFromEnvKey maps an env-var name (DATABASE_URL, REDIS_URL,
// MONGO_URL, ...) to the closed-vocab kind. Returns
// ("", false) when the env key is not in the mapping. The
// classifier at infer.go iterates the env table per-app and
// invokes this for every key.
//
// Distinct from the scheme-based detection below — the env-key
// path is the primary surface (DATABASE_URL → postgres is the
// common case), the scheme-based path is the fallback (an env
// value like CUSTOM_PG_URL=postgres://... still works because
// the scheme is "postgres").
func KindFromEnvKey(envKey string) (string, bool) {
	// The mapping is exact-match on a small set of well-known
	// env keys. A regex would let "DATABASE_URL_FOO" through
	// and the wrong row would land in the table. The closed set
	// is the right shape: PR-A's spec amendment (ADR-098 §D1.a)
	// calls for a small, opinionated list of supported env keys,
	// and the env-classifier defaults to "skip" for unknown keys
	// rather than guessing.
	switch envKey {
	case "DATABASE_URL", "POSTGRES_URL", "POSTGRESQL_URL", "PG_URL",
		"PGHOST", "PGUSER", "PGDATABASE", "PGPORT":
		return kindPostgres, true
	case "REDIS_URL", "REDIS_URL_ALT":
		return kindRedis, true
	case "MONGO_URL", "MONGODB_URL", "MONGODB_URI", "MONGO_URI":
		return "mongo", true
	case "CASSANDRA_URL", "CASSANDRA_CONTACT_POINTS":
		return "cassandra", true
	case "CLICKHOUSE_URL":
		return "clickhouse", true
	case "ELASTICSEARCH_URL", "ES_URL", "ELASTIC_URL":
		return "elasticsearch", true
	case "OPENSEARCH_URL":
		return "opensearch", true
	case "RABBITMQ_URL", "AMQP_URL", "AMQP_URL_ALT":
		return "rabbitmq", true
	case "KAFKA_BROKERS", "KAFKA_URL", "KAFKA_BOOTSTRAP_SERVERS":
		return "kafka", true
	case "NATS_URL":
		return "nats", true
	case "MINIO_URL", "S3_ENDPOINT":
		return "minio", true
	case "S3_BUCKET", "AWS_S3_BUCKET", "S3_URL":
		return "s3", true
	case "MEMCACHED_URL":
		return "memcached", true
	case "ETCD_URL", "ETCD_ENDPOINTS":
		return "etcd", true
	case "API_URL", "EXTERNAL_API_URL", "UPSTREAM_URL":
		return "https_api", true
	}
	return "", false
}

// KindFromScheme is the fallback path. The env key might not be
// in the closed set above, but the value's URL scheme is still
// informative — e.g. CUSTOM_DB_URL=postgres://... resolves to
// "postgres" via the scheme. Returns ("", false) when the scheme
// is unknown.
func KindFromScheme(scheme string) (string, bool) {
	k, ok := schemeKindMap[strings.ToLower(scheme)]
	return k, ok
}

// redisURLPattern is the redis://[user:pass@]host:port/db
// shape. Redis URIs don't always parse cleanly with
// url.Parse (the host may be missing a port) so we hand-roll
// a small parser that's strict about the host + port and
// lenient about the rest.
var redisURLPattern = regexp.MustCompile(`^redis(?:s)?://(?:[^@]+@)?([^/:?#]+)(?::(\d+))?`)

// kafkaBootstrapPattern is the comma-separated host:port list
// (KAFKA_BOOTSTRAP_SERVERS=kafka1:9092,kafka2:9092). We take
// only the first entry — multi-broker is captured later by the
// dashboard's "you have N brokers" rendering.
var kafkaBootstrapPattern = regexp.MustCompile(`^([a-zA-Z0-9.-]+):(\d+)`)

// bareHostPattern matches a hostname with no scheme and no port
// (PGHOST=localhost, REDIS_URL_HOST=cache.example.com). The
// classifier stamps the kind default port at the infer.go layer;
// ExtractHostPort just returns the host with port=0.
var bareHostPattern = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9.-]{0,253}[a-zA-Z0-9])?$`)

// cassandraContactPointsPattern matches comma-separated
// host[:port] entries. Same shape as Kafka.
var cassandraContactPointsPattern = kafkaBootstrapPattern// ExtractHostPort parses a connection-string-shaped env value
// and returns (host, port, kind, ok). The classifier at
// infer.go uses this for URL-shaped values and the
// raw-extractor path for the comma-separated values
// (KAFKA_BOOTSTRAP_SERVERS, CASSANDRA_CONTACT_POINTS).
//
// kind is the closed-vocab value from pkg/api/upstreams.go.
// ok=false means the value is not connection-string-shaped;
// the caller drops it silently.
func ExtractHostPort(raw string) (host string, port int, kind string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", 0, "", false
	}
	// Try URL parse first.
	if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
		host = u.Hostname()
		if p := u.Port(); p != "" {
			if pn, perr := strconv.Atoi(p); perr == nil {
				port = pn
			}
		}
		if k, kok := KindFromScheme(u.Scheme); kok {
			kind = k
		} else if kind == "" {
			// URL parsed but scheme isn't in the closed vocab.
			// Try the env-key path (only meaningful if the
			// caller supplies the key separately).
			return host, port, "", false
		}
		if port == 0 {
			// The env value has no port; fall back to the
			// kind's default (postgres → 5432, redis → 6379, ...).
			// The classifier caller is responsible for stamping
			// the default via api.DefaultPortForKind.
			return host, 0, kind, true
		}
		return host, port, kind, true
	}
	// Hand-roll the redis://host:port shape.
	if m := redisURLPattern.FindStringSubmatch(raw); m != nil {
		host = m[1]
		if m[2] != "" {
			if pn, perr := strconv.Atoi(m[2]); perr == nil {
				port = pn
			}
		}
		return host, port, kindRedis, true
	}
	// Comma-separated bootstrap (kafka, cassandra) — take first.
	if m := kafkaBootstrapPattern.FindStringSubmatch(raw); m != nil {
		host = m[1]
		if pn, perr := strconv.Atoi(m[2]); perr == nil {
			port = pn
		}
		// The kind is unknown until the env key tells us
		// (KAFKA_BOOTSTRAP_SERVERS vs CASSANDRA_CONTACT_POINTS).
		// The caller sets it.
		return host, port, "", true
	}
	// Bare host[:port] (PGHOST without scheme).
	if m := kafkaBootstrapPattern.FindStringSubmatch(raw); m != nil {
		host = m[1]
		if pn, perr := strconv.Atoi(m[2]); perr == nil {
			port = pn
		}
		return host, port, "", true
	}
	// Bare host with no port (PGHOST=localhost, REDIS_HOST=cache).
	// Stamps port=0; the classifier falls back to the kind default.
	if m := bareHostPattern.FindStringSubmatch(raw); m != nil {
		return raw, 0, "", true
	}
	_ = cassandraContactPointsPattern // reserved for future per-kind extractors
	return "", 0, "", false
}

// FormatPortError renders a port-validation error message for
// the apid classifier. Local helper to keep infer.go's error
// messages consistent with the apid-handler error messages.
func FormatPortError(observed int) string {
	return fmt.Sprintf("port %d is outside the [1, 65535] range", observed)
}
